package controller

import (
	"context"
	"testing"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// vsScheme registers the Istio VirtualService kind as unstructured so the
// fake client can store it.
func vsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return sch
}

func vsStack(ns string, vs *platformv1alpha1.StackVirtualService) *platformv1alpha1.Stack {
	return &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Name: "st", Namespace: ns},
		Spec:       platformv1alpha1.StackSpec{VirtualService: vs},
	}
}

func TestEnsureVirtualServiceCreatesAndNamesDefault(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(vsScheme(t)).Build()
	r := &StackReconciler{Client: c, Scheme: c.Scheme()}

	stack := vsStack("tenant-ns", &platformv1alpha1.StackVirtualService{
		Gateway: "gw/gw",
		Host:    "app.example.com",
		HTTP: []apiextensionsv1.JSON{
			{Raw: []byte(`{"name":"root","match":[{"uri":{"prefix":"/"}}],"route":[{"destination":{"host":"frontend","port":3000}}]}`)},
		},
	})
	if err := r.ensureVirtualService(ctx, stack); err != nil {
		t.Fatal(err)
	}

	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(virtualServiceGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: "tenant-ns", Name: "st-routes"}, vs); err != nil {
		t.Fatalf("VirtualService not created: %v", err)
	}
	hosts, _, _ := unstructured.NestedStringSlice(vs.Object, "spec", "hosts")
	if len(hosts) != 1 || hosts[0] != "app.example.com" {
		t.Errorf("hosts = %v", hosts)
	}
	gws, _, _ := unstructured.NestedStringSlice(vs.Object, "spec", "gateways")
	if len(gws) != 1 || gws[0] != "gw/gw" {
		t.Errorf("gateways = %v", gws)
	}
}

func TestEnsureVirtualServiceUpdateOnDrift(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(vsScheme(t)).Build()
	r := &StackReconciler{Client: c, Scheme: c.Scheme()}

	vsSpec := &platformv1alpha1.StackVirtualService{
		Name:    "routes",
		Gateway: "gw/gw",
		Host:    "old.example.com",
		HTTP:    []apiextensionsv1.JSON{{Raw: []byte(`{"route":[{"destination":{"host":"a"}}]}`)}},
	}
	stack := vsStack("tenant-ns", vsSpec)
	if err := r.ensureVirtualService(ctx, stack); err != nil {
		t.Fatal(err)
	}

	// Change the host; ensure must update the existing object.
	stack.Spec.VirtualService.Host = "new.example.com"
	if err := r.ensureVirtualService(ctx, stack); err != nil {
		t.Fatal(err)
	}
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(virtualServiceGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: "tenant-ns", Name: "routes"}, vs); err != nil {
		t.Fatal(err)
	}
	hosts, _, _ := unstructured.NestedStringSlice(vs.Object, "spec", "hosts")
	if len(hosts) != 1 || hosts[0] != "new.example.com" {
		t.Errorf("hosts after update = %v", hosts)
	}
}

func TestEnsureVirtualServiceNoopWithoutSpec(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(vsScheme(t)).Build()
	r := &StackReconciler{Client: c, Scheme: c.Scheme()}
	if err := r.ensureVirtualService(ctx, vsStack("ns", nil)); err != nil {
		t.Errorf("nil virtualService should be a no-op, got %v", err)
	}
}

func TestEnsureVirtualServiceBadRouteJSON(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(vsScheme(t)).Build()
	r := &StackReconciler{Client: c, Scheme: c.Scheme()}
	stack := vsStack("ns", &platformv1alpha1.StackVirtualService{
		Gateway: "gw", Host: "h",
		HTTP: []apiextensionsv1.JSON{{Raw: []byte(`{invalid`)}},
	})
	if err := r.ensureVirtualService(ctx, stack); err == nil {
		t.Error("expected JSON decode error")
	}
}

func TestDeleteConflictingVirtualServices(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(vsScheme(t)).Build()
	r := &StackReconciler{Client: c, Scheme: c.Scheme()}

	mk := func(name string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(virtualServiceGVK)
		u.SetName(name)
		u.SetNamespace("ns")
		u.Object["spec"] = map[string]interface{}{"hosts": []interface{}{"x"}}
		return u
	}
	for _, n := range []string{"backend-mcp", "st-routes"} {
		if err := c.Create(ctx, mk(n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.deleteConflictingVirtualServices(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	// Only the known-conflicting "backend-mcp" is removed; managed VS survive.
	if !vsExists(t, c, "ns", "st-routes") {
		t.Error("managed VS should survive")
	}
	if vsExists(t, c, "ns", "backend-mcp") {
		t.Error("conflicting backend-mcp VS should be deleted")
	}
	// Deleting again is a no-op (NotFound tolerated).
	if err := r.deleteConflictingVirtualServices(ctx, "ns"); err != nil {
		t.Errorf("repeat delete: %v", err)
	}
}

func vsExists(t *testing.T, c client.Client, ns, name string) bool {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(virtualServiceGVK)
	return c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, u) == nil
}

func TestEnsureTenantAgentFWObjects(t *testing.T) {
	ctx := context.Background()
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &StackReconciler{Client: c, Scheme: sch}

	if err := r.ensureTenantAgentFW(ctx, vsStack("tenant-ns", nil)); err != nil {
		t.Fatalf("ensureTenantAgentFW: %v", err)
	}

	if err := c.Get(ctx, client.ObjectKey{Namespace: "tenant-ns", Name: "agentfw"}, &corev1.ConfigMap{}); err != nil {
		t.Errorf("agentfw ConfigMap missing: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "tenant-ns", Name: "agentfw"}, &appsv1.Deployment{}); err != nil {
		t.Errorf("agentfw Deployment missing: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "tenant-ns", Name: "agentfw"}, &corev1.Service{}); err != nil {
		t.Errorf("agentfw Service missing: %v", err)
	}
}
