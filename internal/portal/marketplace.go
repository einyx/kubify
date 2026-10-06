package portal

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"

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
<title>Activate Kubify</title><style>
body{margin:0;background:#f4f2ed;color:#171714;font:16px system-ui,sans-serif}.shell{max-width:700px;margin:10vh auto;padding:48px;background:#fff;border:1px solid #d8d5cd}h1{font-size:42px;letter-spacing:-2px;margin:0 0 16px}p{line-height:1.6;color:#585750}button{background:#171714;color:#fff;border:0;padding:14px 22px;font-weight:650;cursor:pointer}button:disabled{opacity:.45}.meta{margin:28px 0;padding:20px;background:#f4f2ed;white-space:pre-wrap}code{font-size:13px}.error{color:#a12419}</style></head>
<body><main class="shell"><p>MICROSOFT MARKETPLACE</p><h1>Activate your Kubify subscription</h1>
<p id="intro">Validating your Marketplace purchase…</p><div class="meta" id="details">Waiting for purchase token.</div>
<button id="activate" disabled>Activate subscription</button></main><script>
const token=new URLSearchParams(location.search).get('token');const intro=document.querySelector('#intro'),details=document.querySelector('#details'),button=document.querySelector('#activate');let subscription;
async function request(path,body){const r=await fetch(path,{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify(body)});const j=await r.json();if(!r.ok)throw new Error(j.error||'Request failed');return j}
if(!token){intro.textContent='No purchase token was supplied.';intro.className='error'}else request('/api/marketplace/resolve',{token}).then(x=>{subscription=x;intro.textContent='Purchase validated. Confirm activation to start provisioning.';details.textContent='Offer: '+x.offerId+'\nPlan: '+x.planId+'\nSubscription: '+x.id;button.disabled=false}).catch(e=>{intro.textContent='Unable to validate this purchase.';intro.className='error';details.textContent=e.message});
button.onclick=()=>{button.disabled=true;button.textContent='Activating…';request('/api/marketplace/activate',{subscriptionId:subscription.id,planId:subscription.planId,quantity:subscription.quantity}).then(()=>{intro.textContent='Subscription activated. Your Kubify environment is being prepared.';details.textContent='Activation complete.';button.remove()}).catch(e=>{intro.textContent='Activation failed.';intro.className='error';details.textContent=e.message;button.disabled=false;button.textContent='Try again'})};
</script></body></html>`))

func serveMarketplaceLanding(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	marketplaceLanding.Execute(w, nil)
}
