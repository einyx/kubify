// Package portal serves a light operator UI over Stack resources: list
// stacks, inspect component phases, and create new stacks from templates.
package portal

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/einyx/kubo/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/yaml"
)

//go:embed index.html
var indexHTML string

// defaultTemplatesDir is the local template source, relative to the working
// directory. The dir is optional; environment-specific templates live here
// (config/samples/ is gitignored).
const defaultTemplatesDir = "config/samples/portal-templates"

// DefaultTemplatesDir exposes the default for the CLI flag default.
const DefaultTemplatesDir = defaultTemplatesDir

// Portal is the operator portal server.
type Portal struct {
	client          client.Client
	registry        *Registry
	metrics         *Metrics
	agentfwURL      string // raw -agentfw flag value (explicit instance)
	agentfwMu       sync.RWMutex
	agentfwItems    map[string]agentfwItem // product label -> instance
	agentfwOrder    []string
	agentfwExplicit map[string]bool
	agentfwDiscAt   time.Time
	// vaultAddrOverride, when set, replaces the in-cluster Vault address
	// template (http://vault.<ns>.svc:8200) — used for local development
	// against a port-forwarded Vault.
	vaultAddrOverride func(ns string) string

	// restCfg + outOfCluster: when the portal runs outside the cluster
	// (local kubeconfig), Vault calls are proxied through the Kubernetes
	// API server's service proxy — no port-forward needed.
	restCfg      *rest.Config
	outOfCluster bool

	// mcpCaller bridges portal endpoints to the MCP tool dispatch.
	mcpCaller MCPCaller

	// watchClient (set for out-of-cluster runs) watches Stacks so connected
	// browsers get real-time change signals over SSE.
	watchClient client.WithWatch

	// stackWatchMu guards stackWatchers; stackWatchStarted ensures the
	// watch goroutine starts once.
	stackWatchMu      sync.Mutex
	stackWatchers     map[chan struct{}]struct{}
	stackWatchStarted bool
}

// SetVaultAddrFunc overrides how the portal derives the Vault address for a
// namespace. Pass nil to restore the in-cluster default.
func (p *Portal) SetVaultAddrFunc(fn func(ns string) string) { p.vaultAddrOverride = fn }

// New builds a Portal using the provided client and the default template
// sources (embedded built-ins + default local dir; no ConfigMap source).
func New(c client.Client) *Portal {
	return &Portal{client: c, registry: NewRegistry(defaultTemplatesDir, nil), metrics: NewMetrics()}
}

// NewInCluster builds a Portal from the ambient kubeconfig / service account.
// The ConfigMap template source is enabled.
func NewInCluster() (*Portal, error) {
	sch := clientgoscheme.Scheme
	if err := v1alpha1.AddToScheme(sch); err != nil {
		return nil, err
	}
	restCfg, err := config.GetConfig()
	if err != nil {
		return nil, err
	}
	c, err := client.NewWithWatch(restCfg, client.Options{Scheme: sch})
	if err != nil {
		return nil, err
	}
	// Local runs (kubeconfig, no in-cluster env) reach in-cluster Vaults
	// through the API server's service proxy.
	outOfCluster := os.Getenv("KUBERNETES_SERVICE_HOST") == ""
	p := &Portal{
		client:       c,
		watchClient:  c,
		registry:     NewRegistry(defaultTemplatesDir, c),
		metrics:      NewMetrics(),
		restCfg:      restCfg,
		outOfCluster: outOfCluster,
	}
	p.startStackWatch(context.Background())
	return p, nil
}

// StackSummary is one row in the stacks table.
type StackSummary struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Mode       string `json:"mode"`
	Phase      string `json:"phase"`
	Paused     bool   `json:"paused,omitempty"`
	Ready      int    `json:"ready"`
	Total      int    `json:"total"`
	Age        string `json:"age"`
	FailureMsg string `json:"failureMsg,omitempty"`
	// Failing lists the names of components not currently Ready.
	Failing []string `json:"failing,omitempty"`
}

// GetIndexHTML returns the embedded single-page UI.
func (p *Portal) GetIndexHTML() string { return indexHTML }

// agentfwNav returns the nav button that opens the in-SPA Agent traffic
// view. The button is labeled with every discovered product; the view
// itself aggregates and labels each record with its product.
func (p *Portal) agentfwNav() string {
	if !p.AgentfwEnabled() {
		return ""
	}
	labels := p.agentfwLabels()
	dataLabel := strings.Join(labels, ", ")
	switch len(labels) {
	case 0:
		return ""
	case 1:
		return `<button id="afw-nav-btn" data-label="` + labels[0] + `" class="btn secondary" onclick="showAgents()" title="agentfw session archive — ` + labels[0] + `">Agentfw · ` + labels[0] + ` <span class="btn-icon">◉</span></button>`
	default:
		shown := strings.Join(labels[:min(2, len(labels))], ", ")
		if len(labels) > 2 {
			shown += " +" + fmt.Sprint(len(labels)-2)
		}
		return `<button id="afw-nav-btn" data-label="` + dataLabel + `" class="btn secondary" onclick="showAgents()" title="agentfw session archive — ` + dataLabel + `">Agentfw · ` + shown + ` <span class="btn-icon">◉</span></button>`
	}
}

// ListStacks returns every Stack in the cluster, oldest first.
func (p *Portal) ListStacks(ctx context.Context) ([]StackSummary, error) {
	var list v1alpha1.StackList
	if err := p.client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("portal: list stacks: %w", err)
	}
	out := make([]StackSummary, 0, len(list.Items))
	for i := range list.Items {
		s := &list.Items[i]
		sum := StackSummary{
			Namespace: s.Namespace,
			Name:      s.Name,
			Mode:      string(s.Spec.Mode),
			Phase:     s.Status.Phase,
			Paused:    s.Spec.Paused,
			Age:       since(s.CreationTimestamp.Time),
		}
		if sum.Mode == "" {
			sum.Mode = "Direct"
		}
		if sum.Phase == "" {
			sum.Phase = "Pending"
		}
		for _, c := range s.Status.Components {
			sum.Total++
			if c.Phase == v1alpha1.ComponentPhaseReady {
				sum.Ready++
				continue
			}
			sum.Failing = append(sum.Failing, c.Name)
			if c.Phase == v1alpha1.ComponentPhaseFailed && sum.FailureMsg == "" {
				sum.FailureMsg = fmt.Sprintf("%s: %s", c.Name, truncate(c.Message, 160))
			}
		}
		out = append(out, sum)
	}
	return out, nil
}

// StackDetail is the deep view of a single stack.
type StackDetail struct {
	StackSummary
	Operators      map[string]bool `json:"operators,omitempty"`
	Exclude        []string        `json:"exclude,omitempty"`
	Bundle         string          `json:"bundle,omitempty"`
	ValueOverrides []string        `json:"valueOverrides,omitempty"`
	Conditions     []ConditionView `json:"conditions,omitempty"`
	Components     []ComponentView `json:"components"`
}

// ConditionView is one status condition row.
type ConditionView struct {
	Type         string `json:"type"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
	Message      string `json:"message,omitempty"`
	LastTransion string `json:"lastTransition,omitempty"`
}

// ComponentView is one component row.
type ComponentView struct {
	Name           string                    `json:"name"`
	Phase          string                    `json:"phase"`
	Revision       int                       `json:"revision,omitempty"`
	Message        string                    `json:"message,omitempty"`
	Images         []v1alpha1.ComponentImage `json:"images,omitempty"`
	DependsOn      []string                  `json:"dependsOn,omitempty"`
	DependsOnReady []string                  `json:"dependsOnReady,omitempty"`
}

// GetStack returns the component-level status of one stack.
func (p *Portal) GetStack(ctx context.Context, ns, name string) (*StackDetail, error) {
	var s v1alpha1.Stack
	key := types.NamespacedName{Namespace: ns, Name: name}
	if err := p.client.Get(ctx, key, &s); err != nil {
		return nil, err
	}
	d := &StackDetail{
		StackSummary: StackSummary{
			Namespace: ns, Name: name,
			Mode: string(s.Spec.Mode), Phase: s.Status.Phase,
			Paused: s.Spec.Paused,
			Age:    since(s.CreationTimestamp.Time),
		},
		Operators:  map[string]bool{},
		Exclude:    s.Spec.Exclude,
		Components: make([]ComponentView, 0, len(s.Status.Components)),
	}
	overridden := make([]string, 0, len(s.Spec.ComponentValues))
	for comp := range s.Spec.ComponentValues {
		overridden = append(overridden, comp)
	}
	sort.Strings(overridden)
	d.ValueOverrides = overridden
	components := stackComponents(ctx, p.client, &s)
	if s.Spec.Bundle != nil {
		d.Bundle = s.Spec.Bundle.URL
	}
	op := s.Spec.Operators
	if op != nil {
		d.Operators = map[string]bool{
			"agentFW": op.AgentFW, "vault": op.Vault, "istio": op.Istio,
			"spark": op.Spark, "certManager": op.CertManager, "kafka": op.Kafka,
			"postgres": op.Postgres, "kubeflow": op.Kubeflow,
		}
	}
	for _, c := range s.Status.Conditions {
		d.Conditions = append(d.Conditions, ConditionView{
			Type: c.Type, Status: string(c.Status), Reason: c.Reason,
			Message:      truncate(c.Message, 200),
			LastTransion: c.LastTransitionTime.Format("2006-01-02 15:04"),
		})
	}
	if d.Mode == "" {
		d.Mode = "Direct"
	}
	if d.Phase == "" {
		d.Phase = "Pending"
	}
	for _, c := range s.Status.Components {
		d.Total++
		if c.Phase == v1alpha1.ComponentPhaseReady {
			d.Ready++
		} else {
			d.Failing = append(d.Failing, c.Name)
		}
		cv := ComponentView{
			Name:     c.Name,
			Phase:    string(c.Phase),
			Revision: c.Revision,
			Message:  truncate(c.Message, 2000),
			Images:   c.Images,
		}
		// Dependency info lives on the component spec, not the status.
		if comp := findComponent(components, c.Name); comp != nil {
			cv.DependsOn, cv.DependsOnReady = comp.DependsOn, comp.DependsOnReady
		}
		d.Components = append(d.Components, cv)
	}
	return d, nil
}

// stackComponents resolves the component specs behind a Stack: inline
// components when self-contained, otherwise the referenced (cluster-scoped)
// StackDefinition. Missing definitions yield nil — statuses still render
// without dependency info.
func stackComponents(ctx context.Context, c client.Client, s *v1alpha1.Stack) []v1alpha1.StackComponentSpec {
	if s.Spec.Inline != nil {
		return s.Spec.Inline.Components
	}
	if s.Spec.StackRef == "" {
		return nil
	}
	var def v1alpha1.StackDefinition
	if err := c.Get(ctx, types.NamespacedName{Name: s.Spec.StackRef}, &def); err != nil {
		return nil
	}
	return def.Spec.Components
}

func findComponent(comps []v1alpha1.StackComponentSpec, name string) *v1alpha1.StackComponentSpec {
	for i := range comps {
		if comps[i].Name == name {
			return &comps[i]
		}
	}
	return nil
}

// SetTemplateDir overrides the local templates dir (must be called before
// serving).
func (p *Portal) SetTemplateDir(dir string) {
	local := NewRegistry(dir, nil)
	local.client = p.registry.client
	p.registry = local
}

// EnableOutOfCluster marks the portal as running outside the cluster so
// in-cluster Vault calls route through the API server proxy. Must be called
// before serving.
func (p *Portal) EnableOutOfCluster(cfg *rest.Config) {
	p.restCfg = cfg
	p.outOfCluster = true
}

// ListTemplates exposes the template registry for API/MCP consumers.
func (p *Portal) ListTemplates(ctx context.Context) ([]Template, error) {
	return p.registry.List(ctx)
}

// GetStackYAML returns the live Stack manifest as YAML.
func (p *Portal) GetStackYAML(ctx context.Context, ns, name string) (string, error) {
	var s v1alpha1.Stack
	key := types.NamespacedName{Namespace: ns, Name: name}
	if err := p.client.Get(ctx, key, &s); err != nil {
		return "", err
	}
	// Typed objects from the client cache carry no TypeMeta.
	s.APIVersion, s.Kind = v1alpha1.GroupVersion.String(), "Stack"
	b, err := yaml.Marshal(&s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DeleteStack removes the Stack CR. confirm must equal the namespace (typed
// by the operator in the UI). purgeNamespace also deletes the tenant
// namespace; default is to keep it.
func (p *Portal) DeleteStack(ctx context.Context, ns, name, confirm string, purgeNamespace bool) error {
	if confirm != ns {
		return fmt.Errorf("confirmation does not match namespace %q — nothing deleted", ns)
	}
	var s v1alpha1.Stack
	key := types.NamespacedName{Namespace: ns, Name: name}
	if err := p.client.Get(ctx, key, &s); err != nil {
		return err
	}
	if err := p.client.Delete(ctx, &s); err != nil {
		return fmt.Errorf("delete stack: %w", err)
	}
	if purgeNamespace {
		var n corev1.Namespace
		n.Name = ns
		if err := p.client.Delete(ctx, &n); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete namespace: %w", err)
		}
	}
	return nil
}

// CreateRequest is the POST /api/stacks payload.
type CreateRequest struct {
	Template  string          `json:"template"`
	Tenant    string          `json:"tenant"`
	Mode      string          `json:"mode"`
	Exclude   []string        `json:"exclude"`
	Operators map[string]bool `json:"operators"`
}

// CreateFromTemplate renders the chosen template for the given tenant slug
// and creates the Namespace + Stack. Request params override the template
// defaults (mode, exclude, operators). DryRun just returns YAML.
func (p *Portal) CreateFromTemplate(ctx context.Context, req CreateRequest, dryRun bool) (string, error) {
	if req.Template == "" {
		req.Template = "full"
	}
	tmpl, err := p.registry.Get(ctx, req.Template)
	if err != nil {
		return "", err
	}

	tenant := strings.ToLower(strings.TrimSpace(req.Tenant))
	if !validTenant(tenant) {
		return "", fmt.Errorf("tenant must be a lowercase RFC-1123 label (a-z, 0-9, '-')")
	}

	objs, err := renderTemplate(tmpl.Body, tenant)
	if err != nil {
		return "", err
	}

	// Parameter overrides on the Stack object.
	mode := req.Mode
	if mode == "" {
		mode = tmpl.Meta.Defaults.Mode
	}
	exclude := req.Exclude
	if exclude == nil {
		exclude = tmpl.Meta.Defaults.Exclude
	}
	operators := tmpl.Meta.Defaults.Operators
	for k, v := range req.Operators {
		if operators == nil {
			operators = map[string]bool{}
		}
		operators[k] = v
	}
	for i := range objs {
		s, ok := objs[i].(*v1alpha1.Stack)
		if !ok {
			continue
		}
		switch mode {
		case "Flux":
			s.Spec.Mode = v1alpha1.DeploymentModeFlux
		case "", "Direct":
			s.Spec.Mode = v1alpha1.DeploymentModeDirect
		default:
			return "", fmt.Errorf("invalid mode %q (want Direct or Flux)", mode)
		}
		s.Spec.Exclude = exclude
		if operators != nil {
			op := s.Spec.Operators
			if op == nil {
				op = &v1alpha1.ClusterOperators{}
				s.Spec.Operators = op
			}
			applyOperators(op, operators)
		}
	}

	var b strings.Builder
	for i := range objs {
		obj := objs[i]
		b.WriteString("---\n")
		data, err := yaml.Marshal(obj)
		if err != nil {
			return "", err
		}
		b.Write(data)
	}
	if dryRun {
		return b.String(), nil
	}

	for i := range objs {
		obj := objs[i]
		if err := p.client.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			return b.String(), fmt.Errorf("create %s %s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}
	}
	return b.String(), nil
}

// reconcileAnnotation triggers a controller reconcile when changed. It
// mirrors the convention used by the MCP server (internal/mcpserver).
const reconcileAnnotation = "kubify.io/reconcile-at"

// ReconcileStack stamps the reconcile-at annotation on the Stack CR so the
// controller enqueues it immediately.
func (p *Portal) ReconcileStack(ctx context.Context, ns, name string) error {
	var s v1alpha1.Stack
	key := types.NamespacedName{Namespace: ns, Name: name}
	if err := p.client.Get(ctx, key, &s); err != nil {
		return err
	}
	patch := client.MergeFrom(s.DeepCopy())
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	s.Annotations[reconcileAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := p.client.Patch(ctx, &s, patch); err != nil {
		return fmt.Errorf("trigger reconcile: %w", err)
	}
	return nil
}

// PatchRequest is the PATCH /api/stacks/{ns}/{name} payload. Every field is
// optional; only the fields present (non-nil) are applied.
type PatchRequest struct {
	Mode      string          `json:"mode,omitempty"`
	Bundle    *string         `json:"bundle,omitempty"`
	Exclude   []string        `json:"exclude,omitempty"`
	Operators map[string]bool `json:"operators,omitempty"`
	// ComponentValues patches per-component Helm value overrides. A nil
	// value removes the component's override; non-nil replaces it.
	ComponentValues map[string]json.RawMessage `json:"componentValues,omitempty"`
}

// PatchStackSpec applies partial spec updates (mode, bundle, exclude,
// operators) to a live Stack. Passing an empty-string bundle clears it.
func (p *Portal) PatchStackSpec(ctx context.Context, ns, name string, req PatchRequest) error {
	var s v1alpha1.Stack
	key := types.NamespacedName{Namespace: ns, Name: name}
	if err := p.client.Get(ctx, key, &s); err != nil {
		return err
	}
	patch := client.MergeFrom(s.DeepCopy())

	if req.Mode != "" {
		switch req.Mode {
		case "Direct":
			s.Spec.Mode = v1alpha1.DeploymentModeDirect
		case "Flux":
			s.Spec.Mode = v1alpha1.DeploymentModeFlux
		default:
			return fmt.Errorf("invalid mode %q (want Direct or Flux)", req.Mode)
		}
	}
	if req.Bundle != nil {
		url := strings.TrimSpace(*req.Bundle)
		if url != "" && !strings.Contains(url, "://") {
			return fmt.Errorf("bundle %q must be an OCI URL (oci://…)", url)
		}
		if url == "" {
			s.Spec.Bundle = nil
		} else {
			if s.Spec.Bundle == nil {
				s.Spec.Bundle = &v1alpha1.BundleSource{}
			}
			s.Spec.Bundle.URL = url
		}
	}
	if req.Exclude != nil {
		s.Spec.Exclude = req.Exclude
	}
	if req.Operators != nil {
		op := s.Spec.Operators
		if op == nil {
			op = &v1alpha1.ClusterOperators{}
			s.Spec.Operators = op
		}
		applyOperators(op, req.Operators)
	}
	if req.ComponentValues != nil {
		if s.Spec.ComponentValues == nil {
			s.Spec.ComponentValues = map[string]apiextensionsv1.JSON{}
		}
		for comp, raw := range req.ComponentValues {
			if raw == nil {
				delete(s.Spec.ComponentValues, comp)
				continue
			}
			if !json.Valid(raw) {
				return fmt.Errorf("componentValues[%q] is not valid JSON", comp)
			}
			var compact any
			if err := json.Unmarshal(raw, &compact); err != nil {
				return fmt.Errorf("componentValues[%q]: %w", comp, err)
			}
			norm, err := json.Marshal(compact)
			if err != nil {
				return fmt.Errorf("componentValues[%q]: %w", comp, err)
			}
			s.Spec.ComponentValues[comp] = apiextensionsv1.JSON{Raw: norm}
		}
		if len(s.Spec.ComponentValues) == 0 {
			s.Spec.ComponentValues = nil
		}
	}
	if err := p.client.Patch(ctx, &s, patch); err != nil {
		return fmt.Errorf("patch stack: %w", err)
	}
	return nil
}

// atoiDefault parses s as an int, falling back to def when empty/invalid.
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// contextWithTimeout wraps context.WithTimeout for the handlers file.
func contextWithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

// DeleteStackBackup removes a backup record. The copy Job is owned by the
// CR and garbage-collected with it.
func (p *Portal) DeleteStackBackup(ctx context.Context, ns, name string) error {
	var bk v1alpha1.StackBackup
	if err := p.client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &bk); err != nil {
		return err
	}
	if err := p.client.Delete(ctx, &bk); err != nil {
		return fmt.Errorf("delete backup: %w", err)
	}
	return nil
}

// RetryStackBackup creates a fresh copy of a backup's spec; the controller
// skips Succeeded/Failed records, so a retry is a new object.
func (p *Portal) RetryStackBackup(ctx context.Context, ns, name string) (*BackupView, error) {
	var bk v1alpha1.StackBackup
	if err := p.client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &bk); err != nil {
		return nil, err
	}
	retry := &v1alpha1.StackBackup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    bk.Namespace,
			GenerateName: "sbk-",
		},
		Spec: *bk.Spec.DeepCopy(),
	}
	if err := p.client.Create(ctx, retry); err != nil {
		return nil, fmt.Errorf("create backup retry: %w", err)
	}
	return &BackupView{
		Namespace: retry.Namespace,
		Name:      retry.Name,
		Source:    retry.Spec.SourceNamespace,
		Target:    retry.Spec.TargetNamespace,
		Include:   retry.Spec.Include,
		Phase:     "Pending",
		Age:       "0s",
	}, nil
}

// BackupRequest is the POST /api/backups payload.
type BackupRequest struct {
	SourceNamespace string   `json:"sourceNamespace"`
	TargetNamespace string   `json:"targetNamespace"`
	Include         []string `json:"include,omitempty"`
}

// BackupView is one row in the backups list.
type BackupView struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Source    string   `json:"source"`
	Target    string   `json:"target"`
	Include   []string `json:"include,omitempty"`
	Phase     string   `json:"phase"`
	Message   string   `json:"message,omitempty"`
	Age       string   `json:"age"`
}

// CreateStackBackup creates a StackBackup CR in the source namespace; the
// backup controller spawns the copy Job.
func (p *Portal) CreateStackBackup(ctx context.Context, req BackupRequest) (*BackupView, error) {
	if !validTenant(req.SourceNamespace) || !validTenant(req.TargetNamespace) {
		return nil, fmt.Errorf("source and target must be lowercase RFC-1123 namespace names")
	}
	if req.SourceNamespace == req.TargetNamespace {
		return nil, fmt.Errorf("source and target namespace must differ")
	}
	for _, inc := range req.Include {
		if inc != "database" && inc != "s3" {
			return nil, fmt.Errorf("invalid include %q (want database or s3)", inc)
		}
	}
	bk := &v1alpha1.StackBackup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:    req.SourceNamespace,
			GenerateName: "sbk-",
		},
		Spec: v1alpha1.StackBackupSpec{
			SourceNamespace: req.SourceNamespace,
			TargetNamespace: req.TargetNamespace,
			Include:         req.Include,
		},
	}
	if err := p.client.Create(ctx, bk); err != nil {
		return nil, fmt.Errorf("create stackbackup: %w", err)
	}
	return &BackupView{
		Namespace: bk.Namespace,
		Name:      bk.Name,
		Source:    req.SourceNamespace,
		Target:    req.TargetNamespace,
		Include:   req.Include,
		Phase:     "Pending",
		Age:       "0s",
	}, nil
}

// ListStackBackups returns every StackBackup, newest first.
func (p *Portal) ListStackBackups(ctx context.Context) ([]BackupView, error) {
	var list v1alpha1.StackBackupList
	if err := p.client.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("portal: list backups: %w", err)
	}
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[i].CreationTimestamp.Time.After(list.Items[j].CreationTimestamp.Time)
	})
	out := make([]BackupView, 0, len(list.Items))
	for _, bk := range list.Items {
		phase := bk.Status.Phase
		if phase == "" {
			phase = "Pending"
		}
		out = append(out, BackupView{
			Namespace: bk.Namespace,
			Name:      bk.Name,
			Source:    bk.Spec.SourceNamespace,
			Target:    bk.Spec.TargetNamespace,
			Include:   bk.Spec.Include,
			Phase:     phase,
			Message:   truncate(bk.Status.Message, 200),
			Age:       since(bk.CreationTimestamp.Time),
		})
	}
	return out, nil
}

// EventView is one row in the namespace activity feed.
type EventView struct {
	Reason   string `json:"reason"`
	Type     string `json:"type"`
	Object   string `json:"object,omitempty"`
	Message  string `json:"message,omitempty"`
	Count    int32  `json:"count"`
	LastSeen string `json:"lastSeen"`
}

// ComponentPod is one pod belonging to a component's Helm release.
type ComponentPod struct {
	Name     string   `json:"name"`
	Phase    string   `json:"phase"`
	Ready    string   `json:"ready"`
	Restarts int32    `json:"restarts"`
	Age      string   `json:"age"`
	Node     string   `json:"node,omitempty"`
	Images   []string `json:"images,omitempty"`
}

// ListComponentPods returns the pods of a component's Helm release
// (standard helm label app.kubernetes.io/instance=<component>).
func (p *Portal) ListComponentPods(ctx context.Context, ns, component string) ([]ComponentPod, error) {
	if !validTenant(ns) {
		return nil, fmt.Errorf("invalid namespace %q", ns)
	}
	var list corev1.PodList
	sel := client.MatchingLabels{"app.kubernetes.io/instance": component}
	if err := p.client.List(ctx, &list, client.InNamespace(ns), sel); err != nil {
		return nil, fmt.Errorf("portal: list component pods: %w", err)
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	out := make([]ComponentPod, 0, len(list.Items))
	for _, pod := range list.Items {
		ready, total := 0, 0
		var restarts int32
		images := []string{}
		for _, cs := range pod.Status.ContainerStatuses {
			total++
			if cs.Ready {
				ready++
			}
			restarts += cs.RestartCount
		}
		for _, c := range pod.Spec.Containers {
			images = append(images, c.Image)
		}
		phase := string(pod.Status.Phase)
		if phase == "" {
			phase = "Pending"
		}
		out = append(out, ComponentPod{
			Name:     pod.Name,
			Phase:    phase,
			Ready:    fmt.Sprintf("%d/%d", ready, total),
			Restarts: restarts,
			Age:      since(pod.CreationTimestamp.Time),
			Node:     pod.Spec.NodeName,
			Images:   images,
		})
	}
	return out, nil
}

// ListStackEvents returns recent events in the stack's namespace, newest
// first. limit is capped at 50; <=0 uses the cap.
func (p *Portal) ListStackEvents(ctx context.Context, ns string, limit int) ([]EventView, error) {
	var list corev1.EventList
	if err := p.client.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return nil, fmt.Errorf("portal: list events: %w", err)
	}
	sort.Slice(list.Items, func(i, j int) bool {
		ti, tj := list.Items[i].LastTimestamp.Time, list.Items[j].LastTimestamp.Time
		if ti.Equal(tj) {
			return list.Items[i].CreationTimestamp.Time.After(list.Items[j].CreationTimestamp.Time)
		}
		return ti.After(tj)
	})
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	if len(list.Items) > limit {
		list.Items = list.Items[:limit]
	}
	out := make([]EventView, 0, len(list.Items))
	for _, e := range list.Items {
		seen := e.LastTimestamp.Time
		if seen.IsZero() {
			seen = e.CreationTimestamp.Time
		}
		out = append(out, EventView{
			Reason:   e.Reason,
			Type:     e.Type,
			Object:   strings.TrimPrefix(e.InvolvedObject.Kind+"/"+e.InvolvedObject.Name, "/"),
			Message:  truncate(e.Message, 200),
			Count:    e.Count,
			LastSeen: seen.Format("01-02 15:04:05"),
		})
	}
	return out, nil
}

// SetStackPaused patches spec.paused. Paused stops reconciliation without
// deleting deployed components.
func (p *Portal) SetStackPaused(ctx context.Context, ns, name string, paused bool) error {
	var s v1alpha1.Stack
	key := types.NamespacedName{Namespace: ns, Name: name}
	if err := p.client.Get(ctx, key, &s); err != nil {
		return err
	}
	patch := client.MergeFrom(s.DeepCopy())
	s.Spec.Paused = paused
	if err := p.client.Patch(ctx, &s, patch); err != nil {
		return fmt.Errorf("set paused: %w", err)
	}
	return nil
}

// applyOperators sets only the toggles present in the request map.
func applyOperators(op *v1alpha1.ClusterOperators, m map[string]bool) {
	for k, v := range m {
		switch strings.ToLower(k) {
		case "agentfw":
			op.AgentFW = v
		case "vault":
			op.Vault = v
		case "istio":
			op.Istio = v
		case "spark":
			op.Spark = v
		case "certmanager":
			op.CertManager = v
		case "kafka":
			op.Kafka = v
		case "postgres":
			op.Postgres = v
		case "kubeflow":
			op.Kubeflow = v
		}
	}
}

func renderTemplate(body, tenant string) ([]client.Object, error) {
	tmpl, err := template.New("stack").Parse(body)
	if err != nil {
		return nil, err
	}
	var rendered strings.Builder
	if err := tmpl.Execute(&rendered, map[string]string{"Tenant": tenant}); err != nil {
		return nil, err
	}

	docs := strings.Split(rendered.String(), "\n---")
	objs := make([]client.Object, 0, 2)
	for _, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		// Distinguish the Namespace object from the Stack by probing kind.
		var probe struct {
			Kind string `json:"kind"`
		}
		if err := yaml.Unmarshal([]byte(doc), &probe); err != nil {
			return nil, err
		}
		switch probe.Kind {
		case "Namespace":
			var ns corev1.Namespace
			if err := yaml.Unmarshal([]byte(doc), &ns); err != nil {
				return nil, err
			}
			objs = append(objs, &ns)
		case "Stack":
			var stack v1alpha1.Stack
			if err := yaml.Unmarshal([]byte(doc), &stack); err != nil {
				return nil, err
			}
			objs = append(objs, &stack)
		default:
			return nil, fmt.Errorf("portal: unexpected kind %q in template", probe.Kind)
		}
	}
	if len(objs) != 2 {
		return nil, fmt.Errorf("portal: template rendered %d objects, want 2", len(objs))
	}
	return objs, nil
}

func validTenant(s string) bool {
	if s == "" || len(s) > 40 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func since(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// mcpNav returns the nav snippet for the MCP view, or "" when the MCP
// bridge is not wired into this portal.
func (p *Portal) mcpNav() string {
	if p.mcpCaller == nil {
		return ""
	}
	return `<button class="btn secondary" onclick="showMCP()">MCP <span class="btn-icon">⌘</span></button>`
}
