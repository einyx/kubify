package controller

import (
	"context"
	"testing"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func chartWithVersion(v string) *chart.Chart {
	return &chart.Chart{Metadata: &chart.Metadata{Name: "x", Version: v}}
}

func appsv1Deployment() appsv1.Deployment { return appsv1.Deployment{} }
func corev1ConfigMap() corev1.ConfigMap   { return corev1.ConfigMap{} }
func corev1Service() corev1.Service       { return corev1.Service{} }

func fakeClientWith(sch *runtime.Scheme) client.Client {
	return fake.NewClientBuilder().WithScheme(sch).Build()
}

func cmObj() *corev1.ConfigMap   { cm := corev1ConfigMap(); return &cm }
func depObj() *appsv1.Deployment { d := appsv1Deployment(); return &d }
func svcObj() *corev1.Service    { s := corev1Service(); return &s }

func TestIsPendingStatus(t *testing.T) {
	pending := []release.Status{
		release.StatusPendingInstall, release.StatusPendingUpgrade, release.StatusPendingRollback,
	}
	for _, s := range pending {
		if !isPendingStatus(s) {
			t.Errorf("%s should be pending", s)
		}
	}
	for _, s := range []release.Status{release.StatusDeployed, release.StatusFailed, release.StatusUninstalled} {
		if isPendingStatus(s) {
			t.Errorf("%s should not be pending", s)
		}
	}
}

func TestChartVersion(t *testing.T) {
	ch := chartWithVersion("1.2.3")
	if v := chartVersion(ch); v != "1.2.3" {
		t.Errorf("chartVersion = %q", v)
	}
	ch.Metadata.Version = ""
	if v := chartVersion(ch); v != "" {
		t.Errorf("empty version = %q", v)
	}
}

func TestContainsIgnoreCaseAndEqualFold(t *testing.T) {
	if !containsIgnoreCase("Hello World", "WORLD") {
		t.Error("containsIgnoreCase should ignore case")
	}
	if containsIgnoreCase("Hello", "xyz") {
		t.Error("containsIgnoreCase false positive")
	}
	if !equalFold("ABC", "abc") {
		t.Error("equalFold should ignore case")
	}
	if equalFold("ABC", "ABD") {
		t.Error("equalFold false positive")
	}
}

func TestHelmRepoNameStableAndDistinct(t *testing.T) {
	a := helmRepoName("https://charts.example.com")
	b := helmRepoName("https://charts.example.com")
	if a != b {
		t.Error("same repo URL must map to the same HelmRepo name")
	}
	if a == helmRepoName("https://other.example.com") {
		t.Error("different repo URLs must map to different names")
	}
	if len(a) != len("kubo-")+8 {
		t.Errorf("name %q should be kubo- + 8 hex chars", a)
	}
}

func TestToJSONValues(t *testing.T) {
	if v, _ := toJSONValues(nil); v != nil {
		t.Error("nil map should give nil JSON")
	}
	if v, _ := toJSONValues(map[string]interface{}{}); v != nil {
		t.Error("empty map should give nil JSON")
	}
	v, err := toJSONValues(map[string]interface{}{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Raw) != `{"a":1}` {
		t.Errorf("raw = %s", v.Raw)
	}
}

func TestMetaFindCondition(t *testing.T) {
	conds := []metav1.Condition{
		{Type: "Ready", Status: "True"},
		{Type: "Healthy", Status: "False"},
	}
	if c := metaFindCondition(conds, "Healthy"); c == nil || c.Status != "False" {
		t.Error("Healthy condition not found")
	}
	if c := metaFindCondition(conds, "Missing"); c != nil {
		t.Error("missing condition should return nil")
	}
}

func TestGitHostAndTrimPrefix(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://github.com/org/repo.git", "github.com/org/repo.git"},
		{"http://github.com/org/repo", "github.com/org/repo"},
		{"git@github.com:org/repo.git", "git@github.com:org/repo.git"},
	}
	for _, tt := range tests {
		if got := gitHost(tt.in); got != tt.want {
			t.Errorf("gitHost(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := trimPrefix("https://x", "http://"); got != "https://x" {
		t.Errorf("trimPrefix non-match = %q", got)
	}
}

func TestDeleteAgentFW(t *testing.T) {
	ctx := context.Background()
	sch := deployScheme(t)
	c := fakeClientWith(sch)
	r := &StackReconciler{Client: c, Scheme: sch}

	stack := vsStack("tenant-ns", nil)
	if err := r.ensureTenantAgentFW(ctx, stack); err != nil {
		t.Fatal(err)
	}
	if err := r.deleteAgentFW(ctx, stack); err != nil {
		t.Fatalf("deleteAgentFW: %v", err)
	}
	for _, get := range []func() error{
		func() error { return c.Get(ctx, client.ObjectKey{Namespace: "tenant-ns", Name: "agentfw"}, depObj()) },
		func() error { return c.Get(ctx, client.ObjectKey{Namespace: "tenant-ns", Name: "agentfw"}, svcObj()) },
	} {
		if err := get(); err == nil {
			t.Error("agentfw object still exists after deleteAgentFW")
		}
	}
	// The ConfigMap is intentionally kept (user policy customisations).
	if err := c.Get(ctx, client.ObjectKey{Namespace: "tenant-ns", Name: "agentfw"}, cmObj()); err != nil {
		t.Error("agentfw ConfigMap should survive deleteAgentFW")
	}
	// Deleting when nothing exists is not an error.
	if err := r.deleteAgentFW(ctx, stack); err != nil {
		t.Errorf("repeat deleteAgentFW: %v", err)
	}
}
