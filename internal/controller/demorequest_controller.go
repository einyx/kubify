// DemoRequest controller: reconciles website "request demo" submissions into
// tenant Stacks provisioned from portal templates, emails the requester when
// the tenant is Ready, and cleans up when the TTL expires.
package controller

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	"github.com/einyx/kubo/internal/portal"
)

const demoRequestFinalizer = "platform.kubo.io/demorequest-cleanup"

// maxConcurrentDemoReconciles keeps a burst of website submissions from
// stampeding the cluster.
const maxConcurrentDemoReconciles = 2

// DemoRequestReconciler provisions demo tenants from portal templates.
type DemoRequestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Registry resolves portal templates (embedded + ConfigMap sources).
	Registry *portal.Registry
	// Emailer notifies requesters; nil disables notification (phase still
	// advances — the URL is visible on the DemoRequest status).
	Emailer DemoEmailer
	// MaxTenants caps concurrent live demo tenants; 0 = unlimited.
	MaxTenants int
	// Target is the cluster/operator identity this controller handles.
	// Only DemoRequests whose spec.target matches (or both are empty)
	// are reconciled; everything else is skipped so multiple controllers
	// on different clusters can share a single kubo-system namespace.
	Target string
	// DefaultTemplate is used when spec.template is empty.
	DefaultTemplate string
	// TenantDomain forms the public URL: https://<tenant>.<domain>.
	TenantDomain string
}

// DemoEmailer sends the "your demo is ready" message.
type DemoEmailer interface {
	SendDemoReady(ctx context.Context, to, tenant, url string) error
}

// slugRe strips everything unsuitable for an RFC-1123 label.
var slugRe = regexp.MustCompile(`[^a-z0-9-]+`)

// tenantSlug derives a deterministic tenant slug: the company name when
// given, else the email local part; disambiguated by a short email hash so
// two different requesters named "acme" never collide and one requester
// always maps to the same tenant.
func tenantSlug(company, email string) string {
	base := company
	if base == "" {
		if addr, err := mail.ParseAddress(email); err == nil {
			base = strings.Split(addr.Address, "@")[0]
		} else {
			base = email
		}
	}
	base = slugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(base)), "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "demo"
	}
	if len(base) > 30 {
		base = base[:30]
	}
	sum := sha256.Sum256([]byte("demo:" + email))
	return fmt.Sprintf("%s-%x", base, sum[:2])
}

// +kubebuilder:rbac:groups=platform.kubo.io,resources=demorequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.kubo.io,resources=demorequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.kubo.io,resources=stacks,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;create;delete

// Reconcile drives the demo lifecycle: admit → provision → notify → expire.
func (r *DemoRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var dr platformv1alpha1.DemoRequest
	if err := r.Get(ctx, req.NamespacedName, &dr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Skip requests targeted at a different operator/cluster.
	if dr.Spec.Target != r.Target {
		return ctrl.Result{}, nil
	}

	if !dr.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &dr)
	}

	if !controllerutil.ContainsFinalizer(&dr, demoRequestFinalizer) {
		controllerutil.AddFinalizer(&dr, demoRequestFinalizer)
		if err := r.Update(ctx, &dr); err != nil {
			return ctrl.Result{}, err
		}
	}

	if dr.Status.ObservedGeneration != dr.Generation || dr.Status.Phase == "" {
		dr.Status.ObservedGeneration = dr.Generation
		if err := r.setStatus(ctx, &dr, platformv1alpha1.DemoRequestPending, ""); err != nil {
			return ctrl.Result{}, err
		}
	}
	if dr.Status.Phase == platformv1alpha1.DemoRequestExpired {
		return ctrl.Result{}, nil
	}

	// Approval gate: nothing provisions until an operator approves the
	// request in the portal. Phase stays Pending with an explanatory
	// message; the spec flip re-triggers the watch, no polling needed.
	if !dr.Spec.Approved {
		return ctrl.Result{}, r.setStatus(ctx, &dr, platformv1alpha1.DemoRequestPending, "awaiting approval")
	}
	if dr.Status.ApprovedAt == "" {
		dr.Status.ApprovedAt = time.Now().UTC().Format(time.RFC3339)
		if err := r.Status().Update(ctx, &dr); err != nil {
			return ctrl.Result{}, err
		}
	}

	// The tenant slug is pinned at admission: later spec edits (e.g. an
	// operator fixing a typo in company via the portal) must NOT re-derive
	// it, or the reconciler would provision a second namespace and orphan
	// the first.
	tenant := tenantSlug(dr.Spec.Company, dr.Spec.Email)
	if dr.Status.Tenant != "" {
		tenant = dr.Status.Tenant
	}
	if dr.Status.Tenant == "" {
		if err := r.admit(ctx, &dr); err != nil {
			return ctrl.Result{}, err
		}
		if dr.Status.Phase == platformv1alpha1.DemoRequestFailed {
			return ctrl.Result{}, nil // capacity/abuse rejections are terminal
		}
	}

	// Provision (idempotent): render the template and create the objects.
	if err := r.provision(ctx, &dr, tenant); err != nil {
		if err == errNamespaceTerminating {
			// A previous demo with this slug is being cleaned up; retry.
			return ctrl.Result{RequeueAfter: 10 * time.Second}, r.setStatus(ctx, &dr,
				platformv1alpha1.DemoRequestPending, "previous tenant with this slug is terminating")
		}
		log.Error(err, "demo provisioning failed", "tenant", tenant)
		return ctrl.Result{}, r.setStatus(ctx, &dr, platformv1alpha1.DemoRequestFailed, truncStr(err.Error(), 300))
	}

	// Mirror the Stack phase.
	var stack platformv1alpha1.Stack
	err := r.Get(ctx, types.NamespacedName{Namespace: tenant, Name: "product"}, &stack)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	switch stack.Status.Phase {
	case "Ready":
		if dr.Status.NotifiedAt == "" && r.Emailer != nil {
			if err := r.Emailer.SendDemoReady(ctx, dr.Spec.Email, tenant, dr.Status.URL); err != nil {
				log.Error(err, "demo ready email failed", "to", dr.Spec.Email)
				return ctrl.Result{RequeueAfter: time.Minute}, r.setStatus(ctx, &dr,
					platformv1alpha1.DemoRequestFailed, "email failed: "+truncStr(err.Error(), 200))
			}
		}
		dr.Status.NotifiedAt = time.Now().UTC().Format(time.RFC3339)
		if err := r.setStatus(ctx, &dr, platformv1alpha1.DemoRequestReady, ""); err != nil {
			return ctrl.Result{}, err
		}
	case "Failed", "Degraded":
		msg := "tenant deployment failed"
		for _, c := range stack.Status.Conditions {
			if c.Message != "" {
				msg = c.Message
				break
			}
		}
		return ctrl.Result{RequeueAfter: time.Minute}, r.setStatus(ctx, &dr,
			platformv1alpha1.DemoRequestProvisioning, truncStr(msg, 300))
	default:
		if err := r.setStatus(ctx, &dr, platformv1alpha1.DemoRequestProvisioning, ""); err != nil {
			return ctrl.Result{}, err
		}
	}

	// TTL: requeue at the deadline and clean up. Defaults to 72h — the
	// ready email promises automatic removal, so an unset TTL must still
	// expire, never leak a tenant.
	ttl := 72 * time.Hour
	if dr.Spec.TTL != nil && dr.Spec.TTL.Duration > 0 {
		ttl = dr.Spec.TTL.Duration
	}
	// The TTL window is the tenant's lifetime: count from approval, not
	// creation, so time spent awaiting approval doesn't eat the demo.
	base := dr.CreationTimestamp.Time
	if dr.Status.ApprovedAt != "" {
		if at, err := time.Parse(time.RFC3339, dr.Status.ApprovedAt); err == nil {
			base = at
		}
	}
	deadline := base.Add(ttl)
	dr.Status.ExpiresAt = deadline.UTC().Format(time.RFC3339)
	if err := r.Status().Update(ctx, &dr); err != nil {
		return ctrl.Result{}, err
	}
	if time.Now().After(deadline) {
		return ctrl.Result{}, r.expire(ctx, &dr)
	}
	// Requeue at the sooner of the TTL deadline and the mirror cadence:
	// the phase must keep tracking the Stack (Ready flip → notify), so a
	// 72h deadline must not suppress the 1-minute mirror.
	requeue := time.Until(deadline)
	if requeue > time.Minute {
		requeue = time.Minute
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// admit enforces capacity caps and records the tenant/URL.
func (r *DemoRequestReconciler) admit(ctx context.Context, dr *platformv1alpha1.DemoRequest) error {
	if r.MaxTenants > 0 {
		var list platformv1alpha1.DemoRequestList
		if err := r.List(ctx, &list); err != nil {
			return err
		}
		live := 0
		for _, other := range list.Items {
			sameObject := other.Namespace == dr.Namespace && other.Name == dr.Name
			if !sameObject && other.Status.Phase != platformv1alpha1.DemoRequestExpired {
				live++
			}
		}
		if live >= r.MaxTenants {
			return r.setStatus(ctx, dr, platformv1alpha1.DemoRequestFailed,
				fmt.Sprintf("demo capacity reached (%d live); contact hello@meshx.io", r.MaxTenants))
		}
	}
	tenant := tenantSlug(dr.Spec.Company, dr.Spec.Email)
	dr.Status.Tenant = tenant
	dr.Status.URL = "https://" + tenant + "." + r.tenantDomain()
	return r.Status().Update(ctx, dr)
}

func (r *DemoRequestReconciler) tenantDomain() string {
	if r.TenantDomain != "" {
		return r.TenantDomain
	}
	return "meshx.foundation"
}

// errNamespaceTerminating marks a tenant whose namespace is mid-deletion
// (e.g. a previous demo with the same deterministic slug just expired):
// the request must wait, not fail.
var errNamespaceTerminating = fmt.Errorf("namespace is terminating")

// provision renders the template and creates Namespace + Stack + extras.
func (r *DemoRequestReconciler) provision(ctx context.Context, dr *platformv1alpha1.DemoRequest, tenant string) error {
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: tenant}, &ns); err == nil && !ns.DeletionTimestamp.IsZero() {
		return errNamespaceTerminating
	}
	tmplID := dr.Spec.Template
	if tmplID == "" {
		tmplID = r.DefaultTemplate
	}
	if tmplID == "" {
		tmplID = "full"
	}
	tmpl, err := r.Registry.Get(ctx, tmplID)
	if err != nil {
		return fmt.Errorf("template %q: %w", tmplID, err)
	}
	objs, err := portal.RenderTemplate(tmpl.Body, tenant)
	if err != nil {
		return err
	}
	for _, obj := range objs {
		if err := r.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s %s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}
	}
	return nil
}

// expire deletes the tenant Stack (its finalizer cascades) and marks the
// request Expired.
func (r *DemoRequestReconciler) expire(ctx context.Context, dr *platformv1alpha1.DemoRequest) error {
	if err := r.deleteTenant(ctx, dr.Status.Tenant); err != nil {
		return err
	}
	return r.setStatus(ctx, dr, platformv1alpha1.DemoRequestExpired, "")
}

// finalize removes the tenant on DemoRequest deletion.
func (r *DemoRequestReconciler) finalize(ctx context.Context, dr *platformv1alpha1.DemoRequest) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(dr, demoRequestFinalizer) {
		return ctrl.Result{}, nil
	}
	if dr.Status.Tenant != "" && dr.Status.Phase != platformv1alpha1.DemoRequestExpired {
		if err := r.deleteTenant(ctx, dr.Status.Tenant); err != nil {
			return ctrl.Result{}, err
		}
	}
	controllerutil.RemoveFinalizer(dr, demoRequestFinalizer)
	if err := r.Update(ctx, dr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *DemoRequestReconciler) deleteTenant(ctx context.Context, tenant string) error {
	if tenant == "" {
		return nil
	}
	// Seed secrets live in kubo-system, outside the tenant namespace, and
	// outlive tenant deletion. Left behind, they poison a later request that
	// derives the same slug: seeding never overwrites existing keys, so the
	// new tenant gets the old tenant's credentials (observed: stale
	// generated MX_DB_PASSWORD breaking AI's postgres login). Remove them —
	// they are per-tenant by name prefix (<tenant>-*) and fully regenerable.
	var seeds corev1.SecretList
	if err := r.List(ctx, &seeds, client.InNamespace("kubo-system")); err != nil {
		return fmt.Errorf("list kubo-system seeds: %w", err)
	}
	prefix := tenant + "-"
	for i := range seeds.Items {
		s := &seeds.Items[i]
		if strings.HasPrefix(s.Name, prefix) {
			if err := r.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete seed %s: %w", s.Name, err)
			}
		}
	}
	var stack platformv1alpha1.Stack
	err := r.Get(ctx, types.NamespacedName{Namespace: tenant, Name: "product"}, &stack)
	if err == nil {
		if err := r.Delete(ctx, &stack); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stack %s: %w", tenant, err)
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	var ns corev1.Namespace
	err = r.Get(ctx, types.NamespacedName{Name: tenant}, &ns)
	if err == nil {
		if err := r.Delete(ctx, &ns); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete namespace %s: %w", tenant, err)
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *DemoRequestReconciler) setStatus(ctx context.Context, dr *platformv1alpha1.DemoRequest, phase platformv1alpha1.DemoRequestPhase, msg string) error {
	if dr.Status.Phase == phase && dr.Status.Message == msg {
		return nil
	}
	dr.Status.Phase = phase
	dr.Status.Message = msg
	return r.Status().Update(ctx, dr)
}

// SetupWithManager registers the controller.
func (r *DemoRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.DemoRequest{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("demorequest").
		Complete(r)
}

// --- ACS email -------------------------------------------------------------

// ACSDemoEmailer sends mail via Azure Communication Services REST using the
// same HMAC scheme as the website's send-email function. Credentials come
// from a Secret with connection-string, sender keys.
type ACSDemoEmailer struct {
	ConnectionString string
	Sender           string
	HTTPClient       *http.Client
}

// SendDemoReady emails the requester their tenant URL.
func (a *ACSDemoEmailer) SendDemoReady(ctx context.Context, to, tenant, tenantURL string) error {
	endpoint, key, err := parseACS(a.ConnectionString)
	if err != nil {
		return err
	}
	api := endpoint + "/emails:send?api-version=2023-03-31"
	subject := "Your meshX demo is ready"
	text := fmt.Sprintf("Hi,\n\nYour meshX demo environment is ready:\n\n  %s\n\nIt will be automatically removed 72 hours after creation.\n\nIf you have any questions, just reply to this email.\n\n— meshX", tenantURL)
	payload := map[string]any{
		"senderAddress": a.Sender,
		"content": map[string]any{
			"subject":   subject,
			"plainText": text,
		},
		"recipients": map[string]any{
			"to": []map[string]any{{"address": to, "displayName": to}},
		},
		"replyTo": []map[string]any{{"address": "hello@meshx.io", "displayName": "meshX"}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := acsSign(req, key, []byte(body)); err != nil {
		return err
	}
	hc := a.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("acs email: HTTP %d", resp.StatusCode)
	}
	return nil
}

func parseACS(cs string) (endpoint, key string, err error) {
	for _, part := range strings.Split(cs, ";") {
		if v, ok := strings.CutPrefix(part, "endpoint="); ok {
			endpoint = strings.TrimRight(v, "/")
		}
		if v, ok := strings.CutPrefix(part, "accesskey="); ok {
			key = v
		}
	}
	if endpoint == "" || key == "" {
		return "", "", fmt.Errorf("acs connection string missing endpoint/accesskey")
	}
	return endpoint, key, nil
}

// acsSign adds the ContentDigest + HMAC Authorization headers per ACS REST.
func acsSign(req *http.Request, keyB64 string, body []byte) error {
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	contentHash := base64.StdEncoding.EncodeToString(digest[:])
	req.Header.Set("x-ms-content-sha256", contentHash)
	date := time.Now().UTC().Format(http.TimeFormat)
	req.Header.Set("Date", date)
	uri, _ := url.Parse(req.URL.String())
	pathAndQuery := uri.Path
	if uri.RawQuery != "" {
		pathAndQuery += "?" + uri.RawQuery
	}
	signed := strings.Join([]string{
		strings.ToUpper(req.Method),
		strings.ToUpper(req.URL.Path),
		"", // empty query component per ACS signing
		contentHash,
		"", "", // content-type handled below
		date,
	}, "\n")
	// ACS expects: VERB\npathAndQuery\n\ncontentHash\ncontentType\ndate\n (host omitted on server verify)
	signed = strings.Join([]string{
		strings.ToUpper(req.Method),
		pathAndQuery,
		"",
		contentHash,
		req.Header.Get("Content-Type"),
		"",
		date,
		"",
	}, "\n")
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(signed))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	req.Header.Set("Authorization", "HMAC-SHA256 SignedHeaders=x-ms-date;host;x-ms-content-sha256&Signature="+sig)
	req.Header.Set("x-ms-date", date)
	return nil
}

// truncStr caps message length for status fields.
func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// DemoEmailerFromSecret builds an ACSDemoEmailer from the
// kubo-system/demo-request-email Secret (connection-string, sender keys).
// Returns nil when the Secret is missing — notification disabled.
func DemoEmailerFromSecret(ctx context.Context, c client.Client) DemoEmailer {
	var s corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: "kubo-system", Name: "demo-request-email"}, &s); err != nil {
		return nil
	}
	cs, sender := string(s.Data["connection-string"]), string(s.Data["sender"])
	if cs == "" || sender == "" {
		return nil
	}
	return &ACSDemoEmailer{ConnectionString: cs, Sender: sender}
}
