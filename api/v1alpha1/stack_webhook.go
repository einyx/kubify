/*
Copyright 2025 The Kubo Authors.
*/

package v1alpha1

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var stacklog = logf.Log.WithName("stack-webhook")

// SetupStackWebhookWithManager registers the webhook with the manager.
func SetupStackWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).For(&Stack{}).
		WithValidator(&StackValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-platform-kubo-io-v1alpha1-stack,mutating=false,failurePolicy=fail,sideEffects=None,groups=platform.kubo.io,resources=stacks,verbs=create;update,versions=v1alpha1,name=vstack.kb.io,admissionReviewVersions=v1

type StackValidator struct{}

var _ webhook.CustomValidator = &StackValidator{}

func (v *StackValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	return validate(obj)
}

func (v *StackValidator) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	return validate(newObj)
}

func (v *StackValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// reservedNamespaces lists namespaces that must never host a tenant Stack.
var reservedNamespaces = map[string]bool{
	"kube-system": true, "kube-public": true, "kube-node-lease": true,
	"kubo-system": true, "operators": true, "istio-system": true,
	"istio-ingress": true, "cert-manager": true, "flux-system": true,
	"vault-system": true, "spark-operator": true, "vault-operator": true,
}

func validate(obj runtime.Object) (admission.Warnings, error) {
	stack, ok := obj.(*Stack)
	if !ok {
		return nil, fmt.Errorf("expected a Stack, got %T", obj)
	}
	stacklog.Info("validating Stack", "name", stack.Name)

	if reservedNamespaces[stack.Namespace] {
		return nil, fmt.Errorf("namespace %q is reserved and cannot host a Stack", stack.Namespace)
	}

	hasRef := stack.Spec.StackRef != ""
	hasInline := stack.Spec.Inline != nil
	hasBundle := stack.Spec.Bundle != nil && stack.Spec.Bundle.URL != ""

	if hasRef && hasInline {
		return nil, fmt.Errorf("spec.stackRef and spec.inline are mutually exclusive")
	}
	if !hasRef && !hasInline && !hasBundle {
		return nil, fmt.Errorf("one of spec.stackRef, spec.inline, or spec.bundle must be set")
	}
	return nil, nil
}
