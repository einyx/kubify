package portal

import (
	"context"
	"testing"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	"github.com/einyx/kubo/internal/marketplace"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCreateAzureMarketplaceRequestIsIdempotent(t *testing.T) {
	p := newFake(t)
	sub := marketplace.Subscription{
		ID: "subscription/with unsafe chars", Name: "Acme", OfferID: "foundation",
		PlanID: "foundation-lite", Quantity: 2,
		Beneficiary: marketplace.Party{TenantID: "azure-tenant"},
		Purchaser:   marketplace.Party{EmailID: "buyer@example.com"},
	}
	first, err := p.createMarketplaceRequest(context.Background(), sub)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.createMarketplaceRequest(context.Background(), sub)
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != second.Name {
		t.Fatalf("request names differ: %q, %q", first.Name, second.Name)
	}
	if first.Spec.Provider != "azure" || first.Spec.Template != "lite" || first.Spec.ProviderTenantID != "azure-tenant" {
		t.Fatalf("incorrect request: %#v", first.Spec)
	}
	var requests platformv1alpha1.MarketplaceRequestList
	if err := p.client.List(context.Background(), &requests, client.InNamespace("kubo-system")); err != nil {
		t.Fatal(err)
	}
	if len(requests.Items) != 1 {
		t.Fatalf("got %d requests, want 1", len(requests.Items))
	}
}

func TestCreateAzureMarketplaceRequestRequiresPurchaser(t *testing.T) {
	p := newFake(t)
	_, err := p.createMarketplaceRequest(context.Background(), marketplace.Subscription{ID: "sub", PlanID: "full"})
	if err == nil {
		t.Fatal("expected missing purchaser email to fail")
	}
}
