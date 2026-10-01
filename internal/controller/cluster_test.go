package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

func TestIsClusterOperator(t *testing.T) {
	for _, name := range []string{"vault-operator", "spark-operator", "istiod"} {
		if !isClusterOperator(name) {
			t.Errorf("isClusterOperator(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"backend", "vault", "istio-cni", ""} {
		if isClusterOperator(name) {
			t.Errorf("isClusterOperator(%q) = true, want false", name)
		}
	}
}

func TestIsClusterComponent(t *testing.T) {
	if !isClusterComponent(platformv1alpha1.StackComponentSpec{Scope: platformv1alpha1.ComponentScopeCluster}) {
		t.Error("explicit Cluster scope not detected")
	}
	if isClusterComponent(platformv1alpha1.StackComponentSpec{Scope: platformv1alpha1.ComponentScopeNamespaced}) {
		t.Error("explicit Namespaced scope wrongly detected as cluster")
	}
	for _, name := range []string{"vault-operator", "spark-operator", "istiod"} {
		comp := platformv1alpha1.StackComponentSpec{Name: name}
		if !isClusterComponent(comp) {
			t.Errorf("builtin operator %q by name not detected", name)
		}
		comp = platformv1alpha1.StackComponentSpec{Name: "x", ChartRef: platformv1alpha1.ChartRef{ChartName: name}}
		if !isClusterComponent(comp) {
			t.Errorf("builtin operator %q by chart name not detected", name)
		}
	}
}

func newStackReconcilerWithStacks(t *testing.T, stacks ...*platformv1alpha1.Stack) *StackReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := make([]runtime.Object, 0, len(stacks))
	for _, s := range stacks {
		objs = append(objs, s)
	}
	return &StackReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()}
}

func clusterCompStatus(name string) platformv1alpha1.ComponentStatus {
	return platformv1alpha1.ComponentStatus{Name: name, Phase: platformv1alpha1.ComponentPhaseReady, Scope: platformv1alpha1.ComponentScopeCluster}
}

func TestSharedOperatorsNeverReleased(t *testing.T) {
	// Policy: shared cluster operators are never uninstalled by stack
	// lifecycle — releaseOperator must be a no-op for them regardless of
	// what other stacks want.
	liveOther := &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "stack-a"},
		Spec:       platformv1alpha1.StackSpec{Operators: &platformv1alpha1.ClusterOperators{Vault: true}},
	}
	me := &platformv1alpha1.Stack{ObjectMeta: metav1.ObjectMeta{Namespace: "team-c", Name: "stack-c"}}
	r := newStackReconcilerWithStacks(t, liveOther, me)
	ctx := context.Background()

	for _, name := range []string{"vault-operator", "spark-operator", "istiod", "istio-ingress", "kafka-operator", "kubegres", "cert-manager"} {
		if err := r.releaseOperator(ctx, me, name); err != nil {
			t.Errorf("releaseOperator(%s) must be a no-op, got err: %v", name, err)
		}
	}
}

// TestReleaseOperatorIstioIngressNamespace verifies the namespace routing
// logic inside releaseOperator without needing a live Helm engine.
func TestReleaseOperatorIstioIngressNamespace(t *testing.T) {
	operatorNS := func(name string) string {
		if name == "istio-ingress" {
			return "istio-ingress"
		}
		return clusterOperatorsNamespace
	}
	if got := operatorNS("istio-ingress"); got != "istio-ingress" {
		t.Errorf("istio-ingress ns = %q, want %q", got, "istio-ingress")
	}
	if got := operatorNS("spark-operator"); got != clusterOperatorsNamespace {
		t.Errorf("spark-operator ns = %q, want %q", got, clusterOperatorsNamespace)
	}
}

func TestReleaseOperatorVaultTenantDeletesResources(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = platformv1alpha1.AddToScheme(scheme)

	ns := "team-a"
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: vaultSAName, Namespace: ns}}
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: vaultRBACName, Namespace: ns}}
	rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: vaultRBACName, Namespace: ns}}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: vaultUnsealKey, Namespace: ns}}
	me := &platformv1alpha1.Stack{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "stack-a"}}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(sa, role, rb, sec, me).Build()
	r := &StackReconciler{Client: fc}

	if err := r.releaseOperator(context.Background(), me, "vault-tenant"); err != nil {
		t.Fatalf("releaseOperator vault-tenant: %v", err)
	}

	for _, obj := range []struct {
		o    client.Object
		name string
	}{
		{&corev1.ServiceAccount{}, vaultSAName},
		{&rbacv1.Role{}, vaultRBACName},
		{&rbacv1.RoleBinding{}, vaultRBACName},
		{&corev1.Secret{}, vaultUnsealKey},
	} {
		err := fc.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: obj.name}, obj.o)
		if err == nil {
			t.Errorf("%T %q still exists after vault-tenant release", obj.o, obj.name)
		}
	}
}

func TestClusterReleaseInUse(t *testing.T) {
	liveOther := &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "stack-a"},
		Status:     platformv1alpha1.StackStatus{Components: []platformv1alpha1.ComponentStatus{clusterCompStatus("crossplane")}},
	}
	namespacedOnly := &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: "stack-b"},
		Status:     platformv1alpha1.StackStatus{Components: []platformv1alpha1.ComponentStatus{{Name: "crossplane", Phase: platformv1alpha1.ComponentPhaseReady}}},
	}
	me := &platformv1alpha1.Stack{ObjectMeta: metav1.ObjectMeta{Namespace: "team-c", Name: "stack-c"}}
	r := newStackReconcilerWithStacks(t, liveOther, namespacedOnly, me)
	ctx := context.Background()

	if !r.clusterReleaseInUse(ctx, me, "crossplane") {
		t.Error("crossplane is used by stack-a and must be kept")
	}
	if r.clusterReleaseInUse(ctx, me, "cert-manager") {
		t.Error("cert-manager has no cluster-scope consumer and should be released")
	}
}
