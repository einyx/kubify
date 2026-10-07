package portal

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	"github.com/einyx/kubo/internal/marketplace"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func (p *Portal) ActivateMarketplace(ctx context.Context, subscriptionID, planID string, _ int32) (*platformv1alpha1.MarketplaceRequest, error) {
	if subscriptionID == "" || planID == "" {
		return nil, fmt.Errorf("subscriptionId and planId are required")
	}
	c, err := p.marketplaceClient(ctx)
	if err != nil {
		return nil, err
	}
	// Read the authoritative subscription before activation. Browser-supplied
	// identity, quantity, and offer fields are never trusted for provisioning.
	sub, err := c.Get(ctx, subscriptionID)
	if err != nil {
		return nil, fmt.Errorf("read subscription before activation: %w", err)
	}
	if sub.ID == "" {
		sub.ID = subscriptionID
	} else if sub.ID != subscriptionID {
		return nil, fmt.Errorf("marketplace returned a different subscription ID")
	}
	if sub.PlanID != planID {
		return nil, fmt.Errorf("subscription plan %q does not match requested plan %q", sub.PlanID, planID)
	}
	if err := c.Activate(ctx, subscriptionID, sub.PlanID, sub.Quantity); err != nil {
		return nil, err
	}
	return p.createMarketplaceRequest(ctx, sub)
}

func (p *Portal) createMarketplaceRequest(ctx context.Context, sub marketplace.Subscription) (*platformv1alpha1.MarketplaceRequest, error) {
	if sub.Purchaser.EmailID == "" {
		return nil, fmt.Errorf("activated subscription has no purchaser email")
	}
	if sub.Quantity < 1 {
		sub.Quantity = 1
	}
	sum := sha256.Sum256([]byte(sub.ID))
	name := fmt.Sprintf("azure-%x", sum[:8])
	templateID := "full"
	if strings.Contains(strings.ToLower(sub.PlanID), "lite") {
		templateID = "lite"
	}
	mr := &platformv1alpha1.MarketplaceRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kubo-system", Name: name,
			Labels: map[string]string{"platform.kubo.io/marketplace-provider": "azure"}},
		Spec: platformv1alpha1.MarketplaceRequestSpec{
			Provider: "azure", SubscriptionID: sub.ID, OfferID: sub.OfferID,
			PlanID: sub.PlanID, Quantity: sub.Quantity,
			ProviderTenantID: sub.Beneficiary.TenantID, PurchaserEmail: sub.Purchaser.EmailID,
			SubscriptionName: sub.Name, Template: templateID,
		},
	}
	if err := p.client.Create(ctx, mr); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create MarketplaceRequest: %w", err)
		}
		var existing platformv1alpha1.MarketplaceRequest
		if err := p.client.Get(ctx, client.ObjectKeyFromObject(mr), &existing); err != nil {
			return nil, err
		}
		if existing.Spec.SubscriptionID != sub.ID || existing.Spec.Provider != "azure" {
			return nil, fmt.Errorf("MarketplaceRequest name collision for subscription")
		}
		return &existing, nil
	}
	return mr, nil
}

type marketplaceWebhook struct {
	ID             string `json:"id"`
	SubscriptionID string `json:"subscriptionId"`
	OfferID        string `json:"offerId"`
	PlanID         string `json:"planId"`
	Action         string `json:"action"`
}

func (p *Portal) handleMarketplaceWebhook(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		http.Error(w, `{"error":"missing bearer token"}`, http.StatusUnauthorized)
		return
	}
	var event marketplaceWebhook
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&event); err != nil {
		http.Error(w, `{"error":"invalid webhook payload"}`, http.StatusBadRequest)
		return
	}
	if event.ID == "" || event.SubscriptionID == "" {
		http.Error(w, `{"error":"operation and subscription IDs are required"}`, http.StatusBadRequest)
		return
	}
	c, err := p.marketplaceClient(r.Context())
	if err != nil {
		respond(w, r, nil, err)
		return
	}
	op, err := c.GetOperation(r.Context(), event.SubscriptionID, event.ID)
	if err != nil {
		respond(w, r, nil, fmt.Errorf("validate marketplace webhook: %w", err))
		return
	}
	if op.ID != event.ID || op.SubscriptionID != event.SubscriptionID || !strings.EqualFold(op.Action, event.Action) {
		http.Error(w, `{"error":"webhook does not match validated operation"}`, http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "operationId": op.ID})
}

var marketplaceLanding = template.Must(template.New("marketplace").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Activate Foundation · meshX</title><meta name="description" content="Activate your Foundation subscription purchased through Microsoft Marketplace.">
<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Cpath fill='%23e34a28' d='M3 6l13 8 13-8v6L16 20 3 12zm0 14l13-8 13 8v6l-13-8-13 8z'/%3E%3C/svg%3E"><style>
@font-face{font-family:'Stack Sans Headline';src:url('/assets/fonts/marketplace-headline.woff2') format('woff2');font-weight:100 900;font-display:swap}@font-face{font-family:'Stack Sans Text';src:url('/assets/fonts/marketplace-text.woff2') format('woff2');font-weight:100 900;font-display:swap}
:root{--background:oklch(98.232436% .002858 84.559);--foreground:oklch(16.028596% .001689 17.333);--card:oklch(100% 0 0);--border:oklch(88.546369% .007127 88.648);--muted-foreground:oklch(48.448583% .009487 99.035);--brand:oklch(57.578104% .212322 32.209);--brand-foreground:oklch(100% 0 0)}*{box-sizing:border-box}body{margin:0;min-height:100dvh;background:var(--background);color:var(--foreground);font:1rem 'Stack Sans Text',sans-serif;-webkit-font-smoothing:antialiased}.skip{position:fixed;left:1rem;top:-4rem;background:var(--foreground);color:var(--card);padding:.75rem 1rem;z-index:2}.skip:focus{top:1rem}.site{min-height:100dvh;display:grid;grid-template-rows:4rem 1fr auto}.nav{border-bottom:1px solid var(--border);display:flex;align-items:center;padding:0 clamp(1.5rem,5vw,4rem)}.brand{display:flex;align-items:center;gap:.7rem;color:var(--foreground);text-decoration:none;font-family:'Stack Sans Headline';font-size:1.35rem;font-weight:600}.mark{width:1.65rem;height:1.65rem;color:var(--brand)}.market{margin-left:auto;color:var(--muted-foreground);font-size:.8rem}.main{width:min(100%,80rem);margin:auto;padding:clamp(3.5rem,9vw,8rem) clamp(1.5rem,5vw,4rem);display:grid;grid-template-columns:minmax(0,1.15fr) minmax(20rem,.85fr);gap:clamp(3rem,8vw,8rem);align-items:center}.eyebrow{margin:0 0 1.25rem;color:var(--brand);font-size:.75rem;font-weight:650;letter-spacing:.12em;text-transform:uppercase}.headline{max-width:12ch;margin:0;font:300 clamp(3.2rem,6vw,6.25rem)/.94 'Stack Sans Headline';letter-spacing:-.055em;text-wrap:balance}.copy{max-width:34rem;margin:2rem 0 0;color:var(--muted-foreground);line-height:1.6;text-wrap:pretty}.proof{max-width:34rem;margin:2rem 0 0;padding:0;display:grid;grid-template-columns:repeat(3,1fr);border-top:1px solid var(--border);list-style:none}.proof li{padding:1rem .9rem 0 0;color:var(--foreground);font-size:.78rem;line-height:1.45}.proof li+li{padding-left:.9rem;border-left:1px solid var(--border)}.proof strong{display:block;margin-bottom:.25rem;font-family:'Stack Sans Headline';font-weight:600}.panel{background:var(--card);border:1px solid var(--border);border-radius:.75rem;padding:clamp(1.5rem,4vw,2.5rem)}.status-label{margin:0 0 .75rem;color:var(--muted-foreground);font-size:.75rem;font-weight:600;letter-spacing:.08em;text-transform:uppercase}.intro{margin:0;font:500 1.15rem/1.45 'Stack Sans Headline'}.meta{min-height:5.5rem;margin:1.75rem 0;padding:1rem;border-top:1px solid var(--border);border-bottom:1px solid var(--border);color:var(--muted-foreground);font:400 .78rem/1.65 ui-monospace,SFMono-Regular,monospace;white-space:pre-wrap;overflow-wrap:anywhere}.button{width:100%;min-height:2.75rem;border:0;border-radius:.375rem;background:var(--brand);color:var(--brand-foreground);font:600 .9rem 'Stack Sans Text';cursor:pointer;transition:transform .15s ease,filter .2s ease}.button:hover:not(:disabled){filter:brightness(.92)}.button:active:not(:disabled){transform:scale(.98)}.button:focus-visible,.brand:focus-visible{outline:3px solid color-mix(in oklab,var(--brand) 35%,transparent);outline-offset:3px}.button:disabled{cursor:not-allowed;opacity:.45}.error{color:#a32d20}.footer{display:flex;justify-content:space-between;gap:1rem;border-top:1px solid var(--border);padding:1.25rem clamp(1.5rem,5vw,4rem);color:var(--muted-foreground);font-size:.75rem}.footer a{color:inherit}@media(max-width:760px){.market{display:none}.main{grid-template-columns:1fr;align-items:start}.headline{font-size:clamp(3rem,15vw,4.75rem)}.proof{grid-template-columns:1fr}.proof li+li{padding-left:0;border-left:0}.footer{flex-direction:column}}
</style></head>
<body><a class="skip" href="#activation">Skip to activation</a><div class="site"><header class="nav"><a class="brand" href="https://meshx.io/" rel="noreferrer" aria-label="meshX home"><svg class="mark" viewBox="0 0 82 116" fill="currentColor" aria-hidden="true"><path d="M82 37V19L41 45 0 18v19l27 17 14 9 14-9zM82 89v18L41 81 0 108V89l27-17 14-9 14 9z"/></svg><span>meshX</span></a><span class="market">Microsoft Marketplace</span></header><main class="main" id="activation"><section><p class="eyebrow">← Foundation for Azure</p><h1 class="headline">Activate your subscription.</h1><p class="copy">meshX turns the context, expertise and decisions your teams create into an intelligence layer that compounds. Confirm your Marketplace purchase to deploy that layer in Azure.</p><ul class="proof"><li><strong>Your intelligence</strong>Context stays in a layer you own.</li><li><strong>Your cloud</strong>Provisioned and managed in Azure.</li><li><strong>Your choice</strong>Models plug in and swap out.</li></ul></section><section class="panel" aria-live="polite"><p class="status-label">Purchase status</p><p class="intro" id="intro">Validating your Marketplace purchase…</p><div class="meta" id="details">Waiting for purchase token.</div>
<button class="button" id="activate" disabled>Activate subscription</button></section></main><footer class="footer"><span>© meshX</span><span><a href="https://meshx.io/privacy">Privacy</a> · <a href="https://meshx.io/terms">Terms</a></span></footer></div><script>
const token=new URLSearchParams(location.search).get('token');const intro=document.querySelector('#intro'),details=document.querySelector('#details'),button=document.querySelector('#activate');let subscription;
async function request(path,body){const r=await fetch(path,{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify(body)});const j=await r.json();if(!r.ok)throw new Error(j.error||'Request failed');return j}
if(!token){intro.textContent='No purchase token was supplied.';intro.className='error'}else request('/api/marketplace/resolve',{token}).then(x=>{subscription=x;intro.textContent='Purchase validated. Confirm activation to start provisioning.';details.textContent='Offer: '+x.offerId+'\nPlan: '+x.planId+'\nSubscription: '+x.id;button.disabled=false}).catch(e=>{intro.textContent='Unable to validate this purchase.';intro.className='error';details.textContent=e.message});
async function watchProvisioning(name){for(;;){await new Promise(r=>setTimeout(r,5000));const r=await fetch('/api/marketplace/requests/'+encodeURIComponent(name)),x=await r.json();if(!r.ok)throw new Error(x.error||'Unable to read provisioning status');details.textContent=x.message||('Provisioning status: '+(x.phase||'Pending'));if(x.phase==='Ready'){intro.textContent='Your Foundation environment is ready.';details.textContent='Open your environment: ';const a=document.createElement('a');a.href=x.url;a.textContent=x.url;details.append(a);return}if(x.phase==='Failed')throw new Error(x.message||'Provisioning failed')}}
button.onclick=()=>{button.disabled=true;button.textContent='Activating…';request('/api/marketplace/activate',{subscriptionId:subscription.id,planId:subscription.planId,quantity:subscription.quantity}).then(x=>{intro.textContent='Subscription activated. Your Foundation environment is being prepared.';details.textContent='Provisioning request: '+x.name;button.remove();return watchProvisioning(x.name)}).catch(e=>{intro.textContent='Activation failed.';intro.className='error';details.textContent=e.message;button.disabled=false;button.textContent='Try again'})};
</script></body></html>`))

func serveMarketplaceLanding(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; font-src 'self'; connect-src 'self' https://cloudflareinsights.com; style-src 'unsafe-inline'; script-src 'unsafe-inline' https://static.cloudflareinsights.com; img-src 'self' data:; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	marketplaceLanding.Execute(w, nil)
}
