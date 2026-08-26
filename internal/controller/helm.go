package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/release"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

// HelmEngine renders and deploys a component chart into a namespace.
// It is product-agnostic: it only knows ChartRef + merged values.
type HelmEngine struct {
	restCfg *rest.Config
}

func NewHelmEngine() (*HelmEngine, error) {
	return &HelmEngine{restCfg: config.GetConfigOrDie()}, nil
}

// cfgFor returns a Helm action.Configuration scoped to the given namespace.
// Release secrets are stored in that namespace, not in "default".
func (h *HelmEngine) cfgFor(namespace string) (*action.Configuration, error) {
	getter := &restConfigGetter{cfg: h.restCfg, namespace: namespace}
	cfg := &action.Configuration{}
	if err := cfg.Init(getter, namespace, "secret", slogInfo); err != nil {
		return nil, fmt.Errorf("helm init for namespace %s: %w", namespace, err)
	}
	return cfg, nil
}

// restConfigGetter implements genericclioptions.RESTClientGetter from a *rest.Config
// without writing any credentials to disk.
type restConfigGetter struct {
	cfg       *rest.Config
	namespace string
}

func (r *restConfigGetter) ToRESTConfig() (*rest.Config, error) { return r.cfg, nil }

func (r *restConfigGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(r.cfg)
	if err != nil {
		return nil, err
	}
	return memory.NewMemCacheClient(dc), nil
}

func (r *restConfigGetter) ToRESTMapper() (meta.RESTMapper, error) {
	dc, err := r.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	return restmapper.NewDeferredDiscoveryRESTMapper(dc), nil
}

func (r *restConfigGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	kc := clientcmdapi.NewConfig()
	cluster := clientcmdapi.NewCluster()
	cluster.Server = r.cfg.Host
	cluster.CertificateAuthorityData = r.cfg.CAData
	cluster.CertificateAuthority = r.cfg.CAFile
	cluster.InsecureSkipTLSVerify = r.cfg.Insecure
	user := clientcmdapi.NewAuthInfo()
	user.Token = r.cfg.BearerToken
	user.TokenFile = r.cfg.BearerTokenFile
	user.ClientCertificateData = r.cfg.CertData
	user.ClientCertificate = r.cfg.CertFile
	user.ClientKeyData = r.cfg.KeyData
	user.ClientKey = r.cfg.KeyFile
	ctx := clientcmdapi.NewContext()
	ctx.Cluster = "kubo"
	ctx.AuthInfo = "kubo"
	ctx.Namespace = r.namespace
	kc.Clusters["kubo"] = cluster
	kc.AuthInfos["kubo"] = user
	kc.Contexts["kubo"] = ctx
	kc.CurrentContext = "kubo"
	return clientcmd.NewDefaultClientConfig(*kc, &clientcmd.ConfigOverrides{})
}

func slogInfo(format string, v ...interface{}) { slog.Info(fmt.Sprintf(format, v...)) }

// EnsureChart pulls the chart and returns the loaded chart. When pullSecret
// is set (a dockerconfigjson Secret in namespace ns), its credentials are
// used for private OCI chart registries.
func (h *HelmEngine) EnsureChart(ref platformv1alpha1.ChartRef, pullSecret, ns string) (*chart.Chart, error) {
	cfg, err := h.cfgFor("default")
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "stack-chart-*")
	if err != nil {
		return nil, err
	}
	pull := action.NewPullWithOpts(action.WithConfig(cfg))
	pull.DestDir = dir
	pull.Version = ref.ChartVersion
	pull.Settings = cli.New()
	if strings.HasPrefix(ref.RepoURL, "oci://") {
		// OCI pulls require a non-nil registry client on the action config.
		// Always create one; log in with credentials when available.
		rc, rerr := registry.NewClient()
		if rerr != nil {
			return nil, fmt.Errorf("create registry client: %w", rerr)
		}
		if user, pass, uerr := dockerAuthFor(h.restCfg, ns, pullSecret, ref.RepoURL); uerr == nil && user != "" {
			host := strings.TrimPrefix(ref.RepoURL, "oci://")
			if i := strings.Index(host, "/"); i >= 0 {
				host = host[:i]
			}
			if lerr := rc.Login(host, registry.LoginOptBasicAuth(user, pass)); lerr != nil {
				slog.Warn("registry login failed", "host", host, "err", lerr.Error())
			}
		}
		cfg.RegistryClient = rc
	} else if user, pass, uerr := dockerAuthFor(h.restCfg, ns, pullSecret, ref.RepoURL); uerr == nil && user != "" {
		pull.Username = user
		pull.Password = pass
	}
	chartArg := ref.ChartName
	if strings.HasPrefix(ref.RepoURL, "oci://") {
		chartArg = strings.TrimSuffix(ref.RepoURL, "/") + "/" + ref.ChartName
	} else {
		pull.RepoURL = ref.RepoURL
	}
	if _, err := pull.Run(chartArg); err != nil {
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

// dockerAuthFor extracts username/password for the registry in repoURL from a
// dockerconfigjson Secret. Returns empty credentials when the secret is unset
// or missing.
func dockerAuthFor(restCfg *rest.Config, ns, secretName, repoURL string) (user, pass string, err error) {
	if secretName == "" {
		return "", "", nil
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return "", "", err
	}
	registryHost := strings.TrimPrefix(repoURL, "oci://")
	if i := strings.Index(registryHost, "/"); i >= 0 {
		registryHost = registryHost[:i]
	}
	ctx := context.Background()
	sec, err := cs.CoreV1().Secrets(ns).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	raw := sec.Data[".dockerconfigjson"]
	if len(raw) == 0 {
		return "", "", nil
	}
	var cfg struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", "", err
	}
	entry, ok := cfg.Auths[registryHost]
	if !ok {
		for host, e := range cfg.Auths {
			if strings.Contains(host, registryHost) || strings.Contains(registryHost, host) {
				entry, ok = e, true
				break
			}
		}
	}
	if !ok {
		return "", "", nil
	}
	user, pass = entry.Username, entry.Password
	if entry.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err != nil {
			return "", "", err
		}
		user, pass, _ = strings.Cut(string(decoded), ":")
	}
	return user, pass, nil
}

// isPendingStatus reports whether a release is stuck mid-operation
// (install/upgrade/rollback that never completed). Helm refuses new
// operations on such releases until they are rolled back or removed.
func isPendingStatus(s release.Status) bool {
	switch s {
	case release.StatusPendingInstall, release.StatusPendingUpgrade, release.StatusPendingRollback:
		return true
	}
	return false
}

// Deploy installs or upgrades the release and returns the resulting release.
func (h *HelmEngine) Deploy(compName, namespace string, ch *chart.Chart, values map[string]interface{}) (*release.Release, error) {
	cfg, err := h.cfgFor(namespace)
	if err != nil {
		return nil, err
	}

	existing, err := getRelease(cfg, compName)
	if err != nil {
		return nil, err
	}

	// Chart-default values carry internal keys (e.g. istio gateway's
	// "_internal_defaults_do_not_set") whose schemas forbid explicit user
	// values. Merged defaults must not leak them back in.
	delete(values, "_internal_defaults_do_not_set")

	// A failed or interrupted (pending-*) release blocks upgrades; recover
	// by rolling back to the last deployed revision, or uninstalling when
	// there is nothing to roll back to, so the next pass is clean.
	if existing != nil && (existing.Info.Status == release.StatusFailed ||
		isPendingStatus(existing.Info.Status)) {
		if isPendingStatus(existing.Info.Status) && existing.Version > 1 {
			rb := action.NewRollback(cfg)
			rb.Wait = false
			rb.Timeout = 2 * time.Minute
			if err := rb.Run(compName); err != nil {
				return nil, fmt.Errorf("helm rollback pending release %s: %w", compName, err)
			}
		} else {
			un := action.NewUninstall(cfg)
			un.IgnoreNotFound = true
			if _, err := un.Run(compName); err != nil {
				return nil, fmt.Errorf("helm uninstall failed release %s: %w", compName, err)
			}
		}
		existing = nil
	}

	if existing == nil {
		inst := action.NewInstall(cfg)
		inst.ReleaseName = compName
		inst.Namespace = namespace
		inst.CreateNamespace = true
		inst.Wait = false
		inst.SkipSchemaValidation = true
		inst.Timeout = 2 * time.Minute
		inst.Version = chartVersion(ch)
		rel, err := inst.Run(ch, values)
		if err != nil {
			return nil, fmt.Errorf("helm install %s: %w", compName, err)
		}
		return rel, nil
	}

	up := action.NewUpgrade(cfg)
	up.Namespace = namespace
	up.Wait = false
	up.Timeout = 2 * time.Minute
	up.SkipSchemaValidation = true
	up.ReuseValues = false
	up.Version = chartVersion(ch)
	rel, err := up.Run(compName, ch, values)
	if err != nil {
		return nil, fmt.Errorf("helm upgrade %s: %w", compName, err)
	}
	return rel, nil
}

// ReleaseStatus returns the current deploy status of a release.
func (h *HelmEngine) ReleaseStatus(name, namespace string) (release.Status, error) {
	cfg, err := h.cfgFor(namespace)
	if err != nil {
		return "", err
	}
	rel, err := getRelease(cfg, name)
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
	cfg, err := h.cfgFor(namespace)
	if err != nil {
		return err
	}

	// A release stuck in pending-install/upgrade/rollback (crashed upgrade,
	// interrupted reconcile) makes helm refuse every action with "another
	// operation is in progress". Dropping the pending record reverts to the
	// last deployed revision so the uninstall can proceed.
	if rel, gerr := getRelease(cfg, name); gerr == nil && rel != nil &&
		strings.HasPrefix(string(rel.Info.Status), "pending-") {
		if _, derr := cfg.Releases.Delete(rel.Name, rel.Version); derr != nil {
			return fmt.Errorf("clear pending release %s: %w", name, derr)
		}
	}

	un := action.NewUninstall(cfg)
	un.IgnoreNotFound = true
	un.Wait = true
	un.Timeout = 5 * time.Minute
	_, err = un.Run(name)
	return err
}

func getRelease(cfg *action.Configuration, name string) (*release.Release, error) {
	st := action.NewStatus(cfg)
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
