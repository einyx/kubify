package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// --- get_stack_yaml ---------------------------------------------------------

func (s *Server) toolGetStackYAML(ctx context.Context, name, namespace string) (interface{}, error) {
	var st platformv1alpha1.Stack
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &st); err != nil {
		return nil, err
	}
	st.APIVersion, st.Kind = platformv1alpha1.GroupVersion.String(), "Stack"
	b, err := yaml.Marshal(&st)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"yaml": string(b)}, nil
}

// --- get_events: recent Warning events for a stack namespace ----------------

func (s *Server) toolGetEvents(ctx context.Context, namespace string, limit int) (interface{}, error) {
	if limit <= 0 || limit > 50 {
		limit = 15
	}
	var list corev1.EventList
	if err := s.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	sort.Slice(list.Items, func(i, j int) bool {
		return list.Items[j].LastTimestamp.Before(&list.Items[i].LastTimestamp)
	})
	type row struct {
		Type      string `json:"type"`
		Reason    string `json:"reason"`
		Object    string `json:"object"`
		Count     int    `json:"count"`
		Message   string `json:"message"`
		LastSeen  string `json:"lastSeen"`
	}
	var rows []row
	for _, e := range list.Items {
		if e.Type != corev1.EventTypeWarning {
			continue
		}
		rows = append(rows, row{
			Type: e.Type, Reason: e.Reason,
			Object: e.InvolvedObject.Kind + "/" + e.InvolvedObject.Name,
			Count:  e.Count, Message: e.Message, LastSeen: e.LastTimestamp.Format("15:04:05"),
		})
		if len(rows) >= limit {
			break
		}
	}
	if rows == nil {
		rows = []row{}
	}
	return map[string]interface{}{"warnings": rows}, nil
}

// --- diagnose_stack: the "why is it Progressing" bundle ---------------------

func (s *Server) toolDiagnoseStack(ctx context.Context, name, namespace string) (interface{}, error) {
	res, err := s.toolGetStack(ctx, name, namespace)
	if err != nil {
		return nil, err
	}
	events, _ := s.toolGetEvents(ctx, namespace, 10)
	return map[string]interface{}{
		"stack":         res,
		"warningEvents": events,
		"hint": "Non-Ready components list their last reconcile error. Combined with recent " +
			"warning events this usually identifies the blocker (helm locks, missing " +
			"secrets, chart pulls).",
	}, nil
}

// --- feature flags ----------------------------------------------------------

// getFlags reads the MX_FF_* env entries for a component from the Stack spec.
// flags: map key→value for entries present; unset catalogue flags are absent.
func (s *Server) toolGetFlags(ctx context.Context, name, namespace, component string) (interface{}, error) {
	var st platformv1alpha1.Stack
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &st); err != nil {
		return nil, err
	}
	flat := flattenValues(st.Spec.ComponentValues, component)
	flags := map[string]string{}
	for k, v := range flat {
		if len(k) > 6 && k[:6] == "MX_FF_" {
			flags[k] = v
		}
	}
	if flags == nil {
		flags = map[string]string{}
	}
	return map[string]interface{}{"component": component, "flags": flags}, nil
}

// setFlags writes MX_FF_* entries into the component's env map in the Stack
// spec. The operator reconciles the change into the component's ConfigMap,
// which rolls the workload — expect a brief restart.
func (s *Server) toolSetFlags(ctx context.Context, name, namespace, component string, flags map[string]string) (interface{}, error) {
	if len(flags) == 0 {
		return nil, fmt.Errorf("no flags provided")
	}
	for k := range flags {
		if len(k) < 6 || k[:6] != "MX_FF_" {
			return nil, fmt.Errorf("flag %q must use the MX_FF_ prefix (add new flags to feature-flags.yaml, not ad-hoc)", k)
		}
	}
	var st platformv1alpha1.Stack
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := s.Client.Get(ctx, key, &st); err != nil {
		return nil, err
	}
	if st.Spec.ComponentValues == nil {
		st.Spec.ComponentValues = map[string]json.RawMessage{}
	}
	var comp struct {
		Env map[string]string `json:"env,omitempty"`
	}
	if len(st.Spec.ComponentValues[component]) > 0 {
		if err := json.Unmarshal(st.Spec.ComponentValues[component], &comp); err != nil {
			return nil, fmt.Errorf("component %q values are not an env map (raw list form unsupported by this tool)", component)
		}
	}
	if comp.Env == nil {
		comp.Env = map[string]string{}
	}
	changed := map[string]string{}
	for k, v := range flags {
		old, existed := comp.Env[k]
		if !existed || old != v {
			changed[k] = v
		}
		comp.Env[k] = v
	}
	blob, err := json.Marshal(comp)
	if err != nil {
		return nil, err
	}
	st.Spec.ComponentValues[component] = blob
	if err := s.Client.Update(ctx, &st); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"updated":  changed,
		"note":     "componentValues updated — the operator will roll " + component + " to apply the flags",
	}, nil
}

// flattenValues merges every env-ish map of a component's values into one
// key→value view (handles env map shapes used by the demo stacks).
func flattenValues(componentValues map[string]json.RawMessage, component string) map[string]string {
	out := map[string]string{}
	var shapes []struct {
		Env map[string]string `json:"env"`
	}
	var cfg struct {
		Env map[string]string `json:"env"`
	}
	var raw json.RawMessage
	if componentValues[component] != nil {
		raw = componentValues[component]
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &shapes[0])
		_ = json.Unmarshal(raw, &cfg)
	}
	for _, m := range []map[string]string{shapes[0].Env, cfg.Env} {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// --- backups ----------------------------------------------------------------

func (s *Server) toolListBackups(ctx context.Context, namespace string) (interface{}, error) {
	var list platformv1alpha1.StackBackupList
	opts := []client.ListOption{}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := s.Client.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("list stackbackups: %w", err)
	}
	type row struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Source    string `json:"source"`
		Target    string `json:"target"`
		Phase     string `json:"phase"`
		Message   string `json:"message,omitempty"`
	}
	rows := []row{}
	for _, b := range list.Items {
		rows = append(rows, row{
			Name: b.Name, Namespace: b.Namespace,
			Source: b.Spec.SourceNamespace, Target: b.Spec.TargetNamespace,
			Phase: b.Status.Phase, Message: b.Status.Message,
		})
	}
	return map[string]interface{}{"backups": rows}, nil
}

func (s *Server) toolCreateBackup(ctx context.Context, name, namespace, source, target string, include []string) (interface{}, error) {
	b := &platformv1alpha1.StackBackup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: platformv1alpha1.StackBackupSpec{
			SourceNamespace: source,
			TargetNamespace: target,
		},
	}
	if len(include) > 0 {
		b.Spec.Include = include
	}
	if err := s.Client.Create(ctx, b); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"created": name,
		"note":    "StackBackup created — watch phase with get_stack_backup; the Job runs in " + source,
	}, nil
}

func (s *Server) toolGetBackup(ctx context.Context, name, namespace string) (interface{}, error) {
	var b platformv1alpha1.StackBackup
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &b); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"name": b.Name, "namespace": b.Namespace,
		"source": b.Spec.SourceNamespace, "target": b.Spec.TargetNamespace,
		"phase": b.Status.Phase, "job": b.Status.JobName,
		"started": b.Status.StartedAt, "completed": b.Status.CompletedAt,
		"message": b.Status.Message,
	}, nil
}

// --- get_vault_health -------------------------------------------------------

func (s *Server) toolGetVaultHealth(ctx context.Context, namespace string) (interface{}, error) {
	var unseal corev1.Secret
	err := s.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "vault-unseal-keys"}, &unseal)
	switch {
	case apierrors.IsNotFound(err):
		return map[string]interface{}{
			"installed": false,
			"note":      "no vault-unseal-keys secret — the Vault is not initialized (or its keys were lost)",
		}, nil
	case err != nil:
		return nil, err
	}
	token := string(unseal.Data["vault-root"])
	return map[string]interface{}{
		"installed": true,
		"tracked":   true,
		"rootTokenTracked": token != "",
		"note":        "use the portal Vault panel for content operations",
	}, nil
}

// --- list_templates / create_stack_from_template -----------------------------

func (s *Server) toolListTemplates(ctx context.Context, registry interface {
	List(ctx context.Context) ([]TemplateMetaLite, error)
}) (interface{}, error) {
	tpls, err := registry.List(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"templates": tpls}, nil
}

// --- delete_stack (guarded) --------------------------------------------------

func (s *Server) toolDeleteStack(ctx context.Context, name, namespace, confirm string) (interface{}, error) {
	if confirm != namespace {
		return nil, fmt.Errorf("confirm must equal the namespace %q — nothing deleted", namespace)
	}
	var st platformv1alpha1.Stack
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &st); err != nil {
		return nil, err
	}
	if err := s.Client.Delete(ctx, &st); err != nil {
		return nil, err
	}
	return map[string]interface{}{"deleted": name, "namespace": namespace,
		"note": "finalizer will uninstall all component releases"}, nil
}

var _ = apierrors.IsNotFound
var _ = sort.Strings
var _ = client.Object(&corev1.Namespace{})
