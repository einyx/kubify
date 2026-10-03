// Package portal serves a light operator UI over Stack resources: list
// stacks, inspect component phases, and create new stacks from templates.
package portal

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/einyx/kubo/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
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
	client   client.Client
	registry *Registry
}

// New builds a Portal using the provided client and the default template
// sources (embedded built-ins + default local dir; no ConfigMap source).
func New(c client.Client) *Portal {
	return &Portal{client: c, registry: NewRegistry(defaultTemplatesDir, nil)}
}

// NewInCluster builds a Portal from the ambient kubeconfig / service account.
// The ConfigMap template source is enabled.
func NewInCluster() (*Portal, error) {
	sch := kubescheme.Scheme
	if err := v1alpha1.AddToScheme(sch); err != nil {
		return nil, err
	}
	c, err := client.New(config.GetConfigOrDie(), client.Options{Scheme: sch})
	if err != nil {
		return nil, err
	}
	return &Portal{client: c, registry: NewRegistry(defaultTemplatesDir, c)}, nil
}

// StackSummary is one row in the stacks table.
type StackSummary struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Mode       string `json:"mode"`
	Phase      string `json:"phase"`
	Ready      int    `json:"ready"`
	Total      int    `json:"total"`
	Age        string `json:"age"`
	FailureMsg string `json:"failureMsg,omitempty"`
}

// GetIndexHTML returns the embedded single-page UI.
func (p *Portal) GetIndexHTML() string { return indexHTML }

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
			}
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
	Operators  map[string]bool `json:"operators,omitempty"`
	Exclude    []string        `json:"exclude,omitempty"`
	Bundle     string          `json:"bundle,omitempty"`
	Conditions []ConditionView `json:"conditions,omitempty"`
	Components []ComponentView `json:"components"`
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
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	Revision int    `json:"revision,omitempty"`
	Message  string `json:"message,omitempty"`
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
			Age: since(s.CreationTimestamp.Time),
		},
		Operators:  map[string]bool{},
		Exclude:    s.Spec.Exclude,
		Components: make([]ComponentView, 0, len(s.Status.Components)),
	}
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
		}
		d.Components = append(d.Components, ComponentView{
			Name: c.Name, Phase: string(c.Phase), Revision: c.Revision, Message: truncate(c.Message, 300),
		})
	}
	return d, nil
}

// SetTemplateDir overrides the local templates dir (must be called before
// serving).
func (p *Portal) SetTemplateDir(dir string) {
	local := NewRegistry(dir, nil)
	local.client = p.registry.client
	p.registry = local
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
		req.Template = "foundation-full"
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
