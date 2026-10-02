// Package portal serves a light operator UI over Stack resources: list
// stacks, inspect component phases, and create new stacks from the
// foundation demo template.
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
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/yaml"
)

//go:embed template.yaml
var templateFS string

//go:embed index.html
var indexHTML string

// Portal is the operator portal server.
type Portal struct {
	client client.Client
}

// New builds a Portal using the provided client.
func New(c client.Client) *Portal { return &Portal{client: c} }

// NewInCluster builds a Portal from the ambient kubeconfig / service account.
func NewInCluster() (*Portal, error) {
	sch := kubescheme.Scheme
	if err := v1alpha1.AddToScheme(sch); err != nil {
		return nil, err
	}
	c, err := client.New(config.GetConfigOrDie(), client.Options{Scheme: sch})
	if err != nil {
		return nil, err
	}
	return &Portal{client: c}, nil
}

// StackSummary is one row in the stacks table.
type StackSummary struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Mode        string `json:"mode"`
	Phase       string `json:"phase"`
	Ready       int    `json:"ready"`
	Total       int    `json:"total"`
	Age         string `json:"age"`
	FailureMsg  string `json:"failureMsg,omitempty"`
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
	Components []ComponentView `json:"components"`
}

// ComponentView is one component row.
type ComponentView struct {
	Name    string `json:"name"`
	Phase   string `json:"phase"`
	Revision int   `json:"revision,omitempty"`
	Message string `json:"message,omitempty"`
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
		Components: make([]ComponentView, 0, len(s.Status.Components)),
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

// CreateFromTemplate renders the foundation demo template for the given
// tenant slug and creates the Namespace + Stack. dryRun just returns YAML.
func (p *Portal) CreateFromTemplate(ctx context.Context, tenant, mode string, dryRun bool) (string, error) {
	tenant = strings.ToLower(strings.TrimSpace(tenant))
	if !validTenant(tenant) {
		return "", fmt.Errorf("tenant must be a lowercase RFC-1123 label (a-z, 0-9, '-')")
	}

	objs, err := renderTemplate(tenant)
	if err != nil {
		return "", err
	}
	for i := range objs {
		if s, ok := objs[i].(*v1alpha1.Stack); ok && mode == "Flux" {
			s.Spec.Mode = v1alpha1.DeploymentModeFlux
		}
	}

	var b strings.Builder
	for i := range objs {
		obj := objs[i]
		b.WriteString("---\n")
		if data, err := yaml.Marshal(obj); err != nil {
			return "", err
		} else {
			b.Write(data)
		}
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

func renderTemplate(tenant string) ([]client.Object, error) {
	tmpl, err := template.New("stack").Parse(templateFS)
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

func since(t time.Time) string {	d := time.Since(t)
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
