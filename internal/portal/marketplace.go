package portal

import (
	"context"
	"fmt"

	"github.com/einyx/kubo/internal/marketplace"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const marketplaceCredentialsSecret = "kubo-marketplace-credentials"

func (p *Portal) marketplaceClient(ctx context.Context) (*marketplace.Client, error) {
	var secret corev1.Secret
	if err := p.client.Get(ctx, client.ObjectKey{Namespace: "kubo-system", Name: marketplaceCredentialsSecret}, &secret); err != nil {
		return nil, fmt.Errorf("marketplace credentials: %w", err)
	}
	return marketplace.New(marketplace.Config{
		TenantID:     string(secret.Data["tenant_id"]),
		ClientID:     string(secret.Data["client_id"]),
		ClientSecret: string(secret.Data["client_secret"]),
	})
}

func (p *Portal) ResolveMarketplace(ctx context.Context, token string) (marketplace.ResolveResponse, error) {
	if token == "" {
		return marketplace.ResolveResponse{}, fmt.Errorf("marketplace purchase token is required")
	}
	c, err := p.marketplaceClient(ctx)
	if err != nil {
		return marketplace.ResolveResponse{}, err
	}
	return c.Resolve(ctx, token)
}

func (p *Portal) ActivateMarketplace(ctx context.Context, subscriptionID, planID string, quantity int32) error {
	if subscriptionID == "" || planID == "" {
		return fmt.Errorf("subscriptionId and planId are required")
	}
	c, err := p.marketplaceClient(ctx)
	if err != nil {
		return err
	}
	return c.Activate(ctx, subscriptionID, planID, quantity)
}
