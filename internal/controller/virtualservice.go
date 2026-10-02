package controller

import (
	"context"
	"encoding/json"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

var virtualServiceGVK = schema.GroupVersionKind{
	Group:   "networking.istio.io",
	Version: "v1",
	Kind:    "VirtualService",
}

// ensureVirtualService creates or updates the Istio VirtualService described
// in spec.virtualService. It is a no-op when the field is absent.
func (r *StackReconciler) ensureVirtualService(ctx context.Context, stack *platformv1alpha1.Stack) error {
	vs := stack.Spec.VirtualService
	if vs == nil {
		return nil
	}

	name := vs.Name
	if name == "" {
		name = stack.Name + "-routes"
	}

	// Decode each HTTPRoute rule from raw JSON.
	routes := make([]interface{}, len(vs.HTTP))
	for i, raw := range vs.HTTP {
		var rule interface{}
		if err := json.Unmarshal(raw.Raw, &rule); err != nil {
			return err
		}
		routes[i] = rule
	}

	spec := map[string]interface{}{
		"hosts":    []interface{}{vs.Host},
		"gateways": []interface{}{vs.Gateway},
		"http":     routes,
	}

	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(virtualServiceGVK)
	u.SetName(name)
	u.SetNamespace(stack.Namespace)

	err := r.Get(ctx, client.ObjectKeyFromObject(u), u)
	if apierrors.IsNotFound(err) {
		u.Object["spec"] = spec
		if err := r.Create(ctx, u); err != nil {
			return err
		}
		return r.deleteConflictingVirtualServices(ctx, stack.Namespace)
	}
	if err != nil {
		return err
	}

	cur, _ := json.Marshal(u.Object["spec"])
	want, _ := json.Marshal(spec)
	if string(cur) == string(want) {
		return nil
	}
	u.Object["spec"] = spec
	if err := r.Update(ctx, u); err != nil {
		return err
	}
	return r.deleteConflictingVirtualServices(ctx, stack.Namespace)
}

// deleteConflictingVirtualServices removes helm-managed VSes that override
// the operator-managed routes VS. The backend chart creates "backend-mcp"
// with prefix "/" when MCP is enabled, which catches all traffic before the
// operator's VS. We delete it after every reconcile so it self-heals.
func (r *StackReconciler) deleteConflictingVirtualServices(ctx context.Context, namespace string) error {
	conflicting := []string{"backend-mcp"}
	for _, vsName := range conflicting {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(virtualServiceGVK)
		obj.SetName(vsName)
		obj.SetNamespace(namespace)
		err := r.Delete(ctx, obj)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
