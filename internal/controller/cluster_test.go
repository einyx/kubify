package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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

func TestOperatorWantedByOther(t *testing.T) {
	liveOther := &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "stack-a"},
		Spec:       platformv1alpha1.StackSpec{Operators: &platformv1alpha1.ClusterOperators{Vault: true}},
	}
	deletedOther := &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "team-b",
			Name:              "stack-b",
			Finalizers:        []string{stackFinalizer},
			DeletionTimestamp: &metav1.Time{},
		},
		Spec: platformv1alpha1.StackSpec{Operators: &platformv1alpha1.ClusterOperators{Vault: true}},
	}
	me := &platformv1alpha1.Stack{ObjectMeta: metav1.ObjectMeta{Namespace: "team-c", Name: "stack-c"}}
	r := newStackReconcilerWithStacks(t, liveOther, deletedOther, me)
	ctx := context.Background()

	if !r.operatorWantedByOther(ctx, me, "vault-operator") {
		t.Error("vault-operator should be kept while stack-a wants it")
	}
	if r.operatorWantedByOther(ctx, me, "spark-operator") {
		t.Error("spark-operator has no other consumer and should be released")
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
