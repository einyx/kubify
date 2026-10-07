package controller

import (
	"context"
	"testing"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestMarketplaceRequestCreatesPermanentApprovedDemoRequest(t *testing.T) {
	sch := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	mr := &platformv1alpha1.MarketplaceRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "azure-123", Namespace: "kubo-system"},
		Spec: platformv1alpha1.MarketplaceRequestSpec{
			Provider: "azure", SubscriptionID: "sub-123", PlanID: "foundation-full",
			PurchaserEmail: "buyer@example.com", SubscriptionName: "Acme", Template: "full",
		},
	}
	c := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&platformv1alpha1.MarketplaceRequest{}, &platformv1alpha1.DemoRequest{}).
		WithObjects(mr).Build()
	r := &MarketplaceRequestReconciler{Client: c, Scheme: sch}
	key := ctrl.Request{NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace}}
	if _, err := r.Reconcile(context.Background(), key); err != nil {
		t.Fatal(err)
	}

	var dr platformv1alpha1.DemoRequest
	if err := c.Get(context.Background(), key.NamespacedName, &dr); err != nil {
		t.Fatal(err)
	}
	if !dr.Spec.Approved || !dr.Spec.SkipNotification {
		t.Fatalf("unexpected delegated request: %#v", dr.Spec)
	}
	if dr.Spec.TTL == nil || dr.Spec.TTL.Duration != 0 {
		t.Fatalf("Marketplace tenant must be permanent: %#v", dr.Spec.TTL)
	}
	if dr.Spec.Email != "buyer@example.com" || dr.Spec.Template != "full" {
		t.Fatalf("purchase fields not propagated: %#v", dr.Spec)
	}

	// A retry must reuse the same child rather than creating another request.
	if _, err := r.Reconcile(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	var list platformv1alpha1.DemoRequestList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("got %d delegated requests, want 1", len(list.Items))
	}
}

func TestMarketplaceRequestMirrorsReadyTenant(t *testing.T) {
	sch := runtime.NewScheme()
	_ = platformv1alpha1.AddToScheme(sch)
	mr := &platformv1alpha1.MarketplaceRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "azure-123", Namespace: "kubo-system"},
		Spec:       platformv1alpha1.MarketplaceRequestSpec{Provider: "azure", PurchaserEmail: "buyer@example.com", Template: "full"},
	}
	dr := &platformv1alpha1.DemoRequest{
		ObjectMeta: metav1.ObjectMeta{Name: mr.Name, Namespace: mr.Namespace},
		Status:     platformv1alpha1.DemoRequestStatus{Phase: platformv1alpha1.DemoRequestReady, Tenant: "acme-ab12", URL: "https://acme-ab12.example.com"},
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithStatusSubresource(&platformv1alpha1.MarketplaceRequest{}, &platformv1alpha1.DemoRequest{}).WithObjects(mr, dr).Build()
	r := &MarketplaceRequestReconciler{Client: c, Scheme: sch}
	key := ctrl.Request{NamespacedName: types.NamespacedName{Name: mr.Name, Namespace: mr.Namespace}}
	if _, err := r.Reconcile(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	var got platformv1alpha1.MarketplaceRequest
	if err := c.Get(context.Background(), key.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != platformv1alpha1.MarketplaceRequestReady || got.Status.URL != dr.Status.URL {
		t.Fatalf("status not mirrored: %#v", got.Status)
	}
}
