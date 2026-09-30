package controller

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/release"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/yaml"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

// HelmEngine renders and deploys a component chart into a namespace.
// It is product-agnostic: it only knows ChartRef + merged values.
type HelmEngine struct {
	cfg *action.Configuration
}

func NewHelmEngine() (*HelmEngine, error) {
	restCfg := config.GetConfigOrDie()

	// Serialize the rest config to a temp kubeconfig so Helm's
	// client-go plumbing can consume it uniformly (in-cluster or local).
	kc := api.NewConfig()
	cluster := api.NewCluster()
	cluster.Server = restCfg.Host
	cluster.CertificateAuthority = restCfg.CAFile
	cluster.CertificateAuthorityData = restCfg.CAData
	cluster.InsecureSkipTLSVerify = restCfg.Insecure
	user := api.NewAuthInfo()
	user.Token = restCfg.BearerToken
	user.TokenFile = restCfg.BearerTokenFile
	user.ClientCertificate = restCfg.CertFile
	user.ClientCertificateData = restCfg.CertData
	user.ClientKey = restCfg.KeyFile
	user.ClientKeyData = restCfg.KeyData
	user.Username = restCfg.Username
	user.Password = restCfg.Password
	ctx := api.NewContext()
	ctx.Cluster = "stack-operator"
	ctx.AuthInfo = "stack-operator"
	kc.Clusters["stack-operator"] = cluster
	kc.AuthInfos["stack-operator"] = user
	kc.CurrentContext = "stack-operator"
	kc.Contexts["stack-operator"] = ctx

	kcBytes, err := clientcmd.Write(*kc)
	if err != nil {
		return nil, fmt.Errorf("serialize kubeconfig: %w", err)
	}
	tmp, err := os.CreateTemp("", "stack-kubeconfig-*")
	if err != nil {
		return nil, err
	}
	if _, err := tmp.Write(kcBytes); err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	cf := genericclioptions.NewConfigFlags(true)
	kcPath := tmp.Name()
	cf.KubeConfig = &kcPath

	h := &HelmEngine{cfg: &action.Configuration{}}
	if err := h.cfg.Init(cf, metav1.NamespaceDefault, "secret", slogInfo); err != nil {
		return nil, fmt.Errorf("helm init: %w", err)
	}
	return h, nil
}

func slogInfo(format string, v ...interface{}) { slog.Info(fmt.Sprintf(format, v...)) }

// EnsureChart pulls the chart and returns the loaded chart.
func (h *HelmEngine) EnsureChart(ref platformv1alpha1.ChartRef) (*chart.Chart, error) {
	dir, err := os.MkdirTemp("", "stack-chart-*")
	if err != nil {
		return nil, err
	}
	pull := action.NewPullWithOpts(action.WithConfig(h.cfg))
	pull.DestDir = dir
	pull.Version = ref.ChartVersion
	pull.Settings = cli.New()
	pull.RepoURL = ref.RepoURL
	if _, err := pull.Run(ref.ChartName); err != nil {
		return nil, fmt.Errorf("pull chart %s/%s:%s: %w", ref.RepoURL, ref.ChartName, ref.ChartVersion, err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.tgz"))
	if err != nil || len(matches) != 1 {
		return nil, fmt.Errorf("pull chart %s: expected 1 chart archive, found %d", ref.ChartName, len(matches))
	}
	ch, err := loader.Load(matches[0])
	if err != nil {
		return nil, fmt.Errorf("load chart %s: %w", matches[0], err)
	}
	return ch, nil
}

// Deploy installs or upgrades the release and returns the resulting release.
func (h *HelmEngine) Deploy(compName, namespace string, ch *chart.Chart, values map[string]interface{}) (*release.Release, error) {
	existing, err := h.getRelease(compName, namespace)
	if err != nil {
		return nil, err
	}

	valsJSON, _ := yaml.Marshal(values)

	if existing == nil {
		inst := action.NewInstall(h.cfg)
		inst.ReleaseName = compName
		inst.Namespace = namespace
		inst.CreateNamespace = true
		inst.Wait = true
		inst.Timeout = 10 * time.Minute
		inst.Version = chartVersion(ch)
		rel, err := inst.Run(ch, values)
		if err != nil {
			return nil, fmt.Errorf("helm install %s: %w (values: %s)", compName, err, valsJSON)
		}
		return rel, nil
	}

	up := action.NewUpgrade(h.cfg)
	up.Namespace = namespace
	up.Wait = true
	up.Timeout = 10 * time.Minute
	up.ReuseValues = false
	up.Version = chartVersion(ch)
	rel, err := up.Run(compName, ch, values)
	if err != nil {
		return nil, fmt.Errorf("helm upgrade %s: %w (values: %s)", compName, err, valsJSON)
	}
	return rel, nil
}

// ReleaseStatus returns the current deploy status of a release.
func (h *HelmEngine) ReleaseStatus(name, namespace string) (release.Status, error) {
	rel, err := h.getRelease(name, namespace)
	if err != nil {
		return "", err
	}
	if rel == nil {
		return "", nil
	}
	return rel.Info.Status, nil
}

// Uninstall removes a release (used on Stack deletion).
func (h *HelmEngine) Uninstall(name, namespace string) error {
	un := action.NewUninstall(h.cfg)
	un.IgnoreNotFound = true
	un.Wait = true
	un.Timeout = 5 * time.Minute
	_, err := un.Run(name)
	return err
}

func (h *HelmEngine) getRelease(name, namespace string) (*release.Release, error) {
	st := action.NewStatus(h.cfg)
	rel, err := st.Run(name)
	if err != nil {
		if isReleaseNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return rel, nil
}

func isReleaseNotFound(err error) bool {
	return err != nil && containsIgnoreCase(err.Error(), "not found")
}

func containsIgnoreCase(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func chartVersion(ch *chart.Chart) string {
	if ch.Metadata != nil {
		return ch.Metadata.Version
	}
	return ""
}
