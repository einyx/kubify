package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	"github.com/einyx/kubo/internal/portal"
)

func TestTenantSlug(t *testing.T) {
	cases := []struct {
		company, email, wantPrefix string
	}{
		{"Acme Corp", "ada@acme.io", "acme-corp-"},
		{"", "ada@example.io", "ada-"},
		{"Weird & Wonderful Ltd!", "x@y.io", "weird-wonderful-ltd-"},
		{"", "UPPER@Example.IO", "upper-"},
	}
	for _, c := range cases {
		got := tenantSlug(c.company, c.email)
		if len(got) < len(c.wantPrefix) || got[:len(c.wantPrefix)] != c.wantPrefix {
			t.Fatalf("tenantSlug(%q,%q) = %q, want prefix %q", c.company, c.email, got, c.wantPrefix)
		}
	}
	// Deterministic: same input, same slug; different email, different slug.
	if tenantSlug("Acme", "a@x.io") != tenantSlug("Acme", "a@x.io") {
		t.Fatal("slug not deterministic")
	}
	if tenantSlug("Acme", "a@x.io") == tenantSlug("Acme", "b@x.io") {
		t.Fatal("different emails must not collide on the same slug")
	}
}

func TestDemoRequestsForStack(t *testing.T) {
	sch := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	matching := &platformv1alpha1.DemoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "matching", Namespace: "kubo-system"},
		Status:     platformv1alpha1.DemoRequestStatus{Tenant: "test-13cd"},
	}
	other := &platformv1alpha1.DemoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "kubo-system"},
		Status:     platformv1alpha1.DemoRequestStatus{Tenant: "other-tenant"},
	}
	r := &DemoRequestReconciler{Client: fake.NewClientBuilder().WithScheme(sch).WithObjects(matching, other).Build()}
	stack := &platformv1alpha1.Stack{ObjectMeta: metav1.ObjectMeta{Name: "product", Namespace: "test-13cd"}}

	requests := r.demoRequestsForStack(context.Background(), stack)
	if len(requests) != 1 || requests[0].Name != "matching" || requests[0].Namespace != "kubo-system" {
		t.Fatalf("demoRequestsForStack() = %#v, want kubo-system/matching", requests)
	}
}

func TestDemoRequestReconcileProvisionsAndMirrors(t *testing.T) {
	sch := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = platformv1alpha1.AddToScheme(sch)

	now := metav1.Now()
	dr := platformv1alpha1.DemoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-test", Namespace: "kubo-system", Generation: 1, CreationTimestamp: now},
		Spec:       platformv1alpha1.DemoRequestSpec{Email: "ada@acme.io", Company: "Acme", Template: "full", Approved: true},
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithStatusSubresource(&platformv1alpha1.DemoRequest{}).WithObjects(&dr).Build()
	r := &DemoRequestReconciler{
		Client:          c,
		Scheme:          sch,
		Registry:        portal.NewRegistry("", c),
		DefaultTemplate: "full",
		MaxTenants:      5,
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "kubo-system", Name: "demo-test"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The embedded full template provisions namespace + Stack; both must exist.
	var updated platformv1alpha1.DemoRequest
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "kubo-system", Name: "demo-test"}, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != platformv1alpha1.DemoRequestProvisioning {
		t.Fatalf("phase = %q, want Provisioning (stack not Ready yet)", updated.Status.Phase)
	}
	tenant := updated.Status.Tenant
	if tenant == "" {
		t.Fatal("status.tenant not set")
	}
	if updated.Status.URL == "" {
		t.Fatal("status.url not set")
	}
	var stack platformv1alpha1.Stack
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: tenant, Name: "product"}, &stack); err != nil {
		t.Fatalf("stack in tenant %s: %v", tenant, err)
	}
}

func TestDemoRequestCapacityCap(t *testing.T) {
	sch := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = platformv1alpha1.AddToScheme(sch)

	existing := platformv1alpha1.DemoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-live", Namespace: "kubo-system"},
		Spec:       platformv1alpha1.DemoRequestSpec{Email: "a@x.io"},
		Status:     platformv1alpha1.DemoRequestStatus{Phase: platformv1alpha1.DemoRequestReady, Tenant: "x"},
	}
	newDr := platformv1alpha1.DemoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-new", Namespace: "kubo-system", Generation: 1},
		Spec:       platformv1alpha1.DemoRequestSpec{Email: "b@y.io", Approved: true},
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithStatusSubresource(&platformv1alpha1.DemoRequest{}).WithObjects(&existing, &newDr).Build()
	// The fake client strips status on seeding for status-subresource types;
	// write it through the subresource so the capacity test sees it live.
	if err := c.Status().Update(context.Background(), &existing); err != nil {
		t.Fatal(err)
	}
	r := &DemoRequestReconciler{
		Client: c, Scheme: sch,
		Registry: portal.NewRegistry("", c), MaxTenants: 1,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "kubo-system", Name: "demo-new"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var updated platformv1alpha1.DemoRequest
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "kubo-system", Name: "demo-new"}, &updated)
	if updated.Status.Phase != platformv1alpha1.DemoRequestFailed {
		t.Fatalf("phase = %q, want Failed (capacity)", updated.Status.Phase)
	}
}

// Regression: editing spec.company after admission must not re-derive the
// tenant slug — the reconciler keeps provisioning into status.tenant, or it
// would spawn a second namespace and orphan the first.
func TestDemoRequestTenantPinnedAcrossCompanyEdit(t *testing.T) {
	sch := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = platformv1alpha1.AddToScheme(sch)

	now := metav1.Now()
	dr := platformv1alpha1.DemoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-edit", Namespace: "kubo-system", Generation: 1, CreationTimestamp: now},
		Spec:       platformv1alpha1.DemoRequestSpec{Email: "ada@acme.io", Company: "Acme", Template: "full", Approved: true},
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithStatusSubresource(&platformv1alpha1.DemoRequest{}).WithObjects(&dr).Build()
	r := &DemoRequestReconciler{
		Client:          c,
		Scheme:          sch,
		Registry:        portal.NewRegistry("", c),
		DefaultTemplate: "full",
		MaxTenants:      5,
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "kubo-system", Name: "demo-edit"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var first platformv1alpha1.DemoRequest
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "kubo-system", Name: "demo-edit"}, &first); err != nil {
		t.Fatal(err)
	}
	if first.Status.Tenant == "" {
		t.Fatal("status.tenant not set on first reconcile")
	}

	// Operator fixes a typo in the company via the portal.
	first.Spec.Company = "Acme Corp Renamed"
	first.Generation = 2
	if err := c.Update(context.Background(), &first); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "kubo-system", Name: "demo-edit"}}); err != nil {
		t.Fatalf("reconcile after edit: %v", err)
	}

	var second platformv1alpha1.DemoRequest
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "kubo-system", Name: "demo-edit"}, &second); err != nil {
		t.Fatal(err)
	}
	if second.Status.Tenant != first.Status.Tenant {
		t.Fatalf("tenant drifted after company edit: %q -> %q", first.Status.Tenant, second.Status.Tenant)
	}
	// No Stack may appear in the re-derived namespace.
	var stranger platformv1alpha1.Stack
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: tenantSlug("Acme Corp Renamed", "ada@acme.io"), Name: "product"}, &stranger); err == nil {
		t.Fatal("stack provisioned into a second namespace derived from the edited company")
	}
}
