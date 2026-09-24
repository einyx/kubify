package controller

import (
	"context"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// publishStackPhase persists lifecycle transitions before potentially slow work.
// ObservedGeneration is advanced only by the completed reconciliation.
func (r *StackReconciler) publishStackPhase(ctx context.Context, stack *platformv1alpha1.Stack, phase, reason, message string) error {
	condition := meta.FindStatusCondition(stack.Status.Conditions, "Ready")
	if stack.Status.Phase == phase && condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == reason && condition.Message == message && condition.ObservedGeneration == stack.Generation {
		return nil
	}
	stack.Status.Phase = phase
	meta.SetStatusCondition(&stack.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: reason,
		Message: message, ObservedGeneration: stack.Generation,
	})
	return r.Status().Update(ctx, stack)
}
