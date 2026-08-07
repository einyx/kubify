package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"helm.sh/helm/v3/pkg/chart"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

// azureDevOpsResource is the resource URI accepted by Azure DevOps for token exchange.
const azureDevOpsResource = "499b84ac-1321-427f-aa17-267ca6975798"

var imdsClient = &http.Client{Timeout: 10 * time.Second}

// chartsFromGitRepository shallow-clones the git repo in spec.gitRef and returns
// the parsed charts from the archive at spec.gitRef.path (default: charts.tgz).
// Results are cached by repo URL + ref so repeated reconciles are cheap.
func (r *StackReconciler) chartsFromGitRepository(ctx context.Context, stack *platformv1alpha1.Stack) (map[string]*chart.Chart, error) {
	ref := stack.Spec.GitRef
	gitRef := ref.Ref
	if gitRef == "" {
		gitRef = "main"
	}
	chartsPath := ref.Path
	if chartsPath == "" {
		chartsPath = "charts.tgz"
	}

	// ponytail: cache key is url+ref — avoids re-cloning on every reconcile
	cacheKey := ref.URL + "@" + gitRef
	if cached, ok := r.bundleCache.Load(cacheKey); ok {
		e := cached.(bundleCacheEntry)
		return e.charts, nil
	}

	dir, err := os.MkdirTemp("", "kubo-git-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	env, cleanup, err := r.gitEnv(ctx, stack)
	if err != nil {
		return nil, err
	}
	if cleanup != nil {
		defer cleanup()
	}

	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", "--branch="+gitRef, "--", ref.URL, dir)
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git clone %s@%s: %w\n%s", ref.URL, gitRef, err, out)
	}

	archivePath := filepath.Join(dir, chartsPath)
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open %s in cloned repo: %w", chartsPath, err)
	}
	defer f.Close()

	chartDir, err := os.MkdirTemp("", "kubo-git-charts-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(chartDir)

	if err := untarGz(f, chartDir); err != nil {
		return nil, fmt.Errorf("unpack %s: %w", chartsPath, err)
	}

	charts, err := loadCharts(chartDir)
	if err != nil {
		return nil, err
	}

	r.bundleCache.Store(cacheKey, bundleCacheEntry{charts: charts, images: map[string]bundleImage{}})
	log.FromContext(ctx).Info("git charts parsed", "url", ref.URL, "ref", gitRef, "charts", len(charts))
	return charts, nil
}

// gitEnv returns env vars for git auth and a cleanup func (may be nil).
func (r *StackReconciler) gitEnv(ctx context.Context, stack *platformv1alpha1.Stack) ([]string, func(), error) {
	ref := stack.Spec.GitRef

	if ref.Provider == "azure" {
		token, err := imdsToken(ctx, azureDevOpsResource)
		if err != nil {
			return nil, nil, fmt.Errorf("azure workload identity token: %w", err)
		}
		// Azure DevOps accepts any non-empty username with a Bearer token as password.
		return gitCredEnv("x-access-token", token)
	}

	if ref.SecretRef == nil || ref.SecretRef.Name == "" {
		return []string{"GIT_TERMINAL_PROMPT=0"}, nil, nil
	}

	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: stack.Namespace, Name: ref.SecretRef.Name}, &secret); err != nil {
		return nil, nil, fmt.Errorf("get git auth secret %s: %w", ref.SecretRef.Name, err)
	}

	env := []string{"GIT_TERMINAL_PROMPT=0"}

	// SSH auth: write identity to a temp file and point GIT_SSH_COMMAND at it.
	if key, ok := secret.Data["identity"]; ok {
		kf, err := os.CreateTemp("", "kubo-git-key-*")
		if err != nil {
			return nil, nil, err
		}
		if err := os.WriteFile(kf.Name(), key, 0o600); err != nil {
			os.Remove(kf.Name())
			return nil, nil, err
		}
		kf.Close()
		env = append(env, fmt.Sprintf(
			"GIT_SSH_COMMAND=ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null",
			kf.Name(),
		))
		return env, func() { os.Remove(kf.Name()) }, nil
	}

	// HTTPS auth via credential store file.
	user := string(secret.Data["username"])
	pass := string(secret.Data["password"])
	if user != "" && pass != "" {
		return gitCredEnv(user, pass)
	}

	return env, nil, nil
}

// gitCredEnv writes an HTTPS credential store file and returns env vars pointing at it.
func gitCredEnv(user, pass string) ([]string, func(), error) {
	cf, err := os.CreateTemp("", "kubo-git-cred-*")
	if err != nil {
		return nil, nil, err
	}
	fmt.Fprintf(cf, "https://%s:%s@\n", user, pass)
	cf.Close()
	os.Chmod(cf.Name(), 0o600)
	env := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=credential.helper",
		fmt.Sprintf("GIT_CONFIG_VALUE_0=store --file=%s", cf.Name()),
	}
	return env, func() { os.Remove(cf.Name()) }, nil
}

// imdsToken fetches an access token from the Azure Instance Metadata Service.
func imdsToken(ctx context.Context, resource string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://169.254.169.254/metadata/identity/oauth2/token", nil)
	if err != nil {
		return "", err
	}
	q := req.URL.Query()
	q.Set("api-version", "2018-02-01")
	q.Set("resource", resource)
	req.URL.RawQuery = q.Encode()
	req.Header.Set("Metadata", "true")

	resp, err := imdsClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("IMDS request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("IMDS returned HTTP %d", resp.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode IMDS response: %w", err)
	}
	if body.AccessToken == "" {
		return "", fmt.Errorf("IMDS returned empty access_token")
	}
	return body.AccessToken, nil
}

// gitHost extracts "host/path" from an https URL for the credential store line.
func gitHost(url string) string {
	s := url
	for _, prefix := range []string{"https://", "http://"} {
		s = trimPrefix(s, prefix)
	}
	return s
}

func trimPrefix(s, prefix string) string {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}
