/*
Copyright 2025 The Kubo Authors.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

const stackFinalizer = "platform.kubo.io/stack-cleanup"

// StackReconciler reconciles a Stack object. It is fully generic: it
// resolves spec.stackRef to a StackDefinition and deploys its components
// as Helm releases. Adding a product = authoring a StackDefinition.
type StackReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Helm   *HelmEngine
}

// +kubebuilder:rbac:groups=platform.kubo.io,resources=stacks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.kubo.io,resources=stacks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.kubo.io,resources=stacks/finalizers,verbs=update
// +kubebuilder:rbac:groups=platform.kubo.io,resources=stackdefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="*",resources="*",verbs="*"

func (r *StackReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var stack platformv1alpha1.Stack
	if err := r.Get(ctx, req.NamespacedName, &stack); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if stack.Spec.Paused {
		return ctrl.Result{}, nil
	}

	if stack.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&stack, stackFinalizer) {
			controllerutil.AddFinalizer(&stack, stackFinalizer)
			return ctrl.Result{}, r.Update(ctx, &stack)
		}
	} else {
		return ctrl.Result{}, r.finalize(ctx, &stack)
	}

	var def platformv1alpha1.StackDefinition
	if err := r.Get(ctx, client.ObjectKey{Name: stack.Spec.StackRef}, &def); err != nil {
		meta.SetStatusCondition(&stack.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse,
			Reason: "StackDefinitionNotFound", Message: err.Error(),
		})
		stack.Status.Phase = "Failed"
		_ = r.Status().Update(ctx, &stack)
		return ctrl.Result{RequeueAfter: time.Minute}, client.IgnoreNotFound(err)
	}

	order, err := topoOrder(def.Spec.Components)
	if err != nil {
		return ctrl.Result{}, r.fail(ctx, &stack, "InvalidStackDefinition", err)
	}

	byName := map[string]platformv1alpha1.StackComponentSpec{}
	for _, c := range def.Spec.Components {
		byName[c.Name] = c
	}

	stack.Status.ObservedGeneration = stack.Generation
	stack.Status.Phase = "Progressing"

	allReady := true
	var firstErr error
	var statuses []platformv1alpha1.ComponentStatus

	for _, name := range order {
		comp := byName[name]
		st := platformv1alpha1.ComponentStatus{Name: name, Phase: "Deploying"}

		chart, err := r.Helm.EnsureChart(comp.ChartRef)
		if err != nil {
			st.Phase, st.Message = "Failed", err.Error()
			statuses = append(statuses, st)
			allReady, firstErr = false, err
			break // dependency order: stop at first failure
		}

		values := resolveComponentValues(&comp.Values, &stack.Spec.Values, stack.Spec.ComponentValues, name)
		rel, err := r.Helm.Deploy(name, stack.Namespace, chart, values)
		if err != nil {
			st.Phase, st.Message = "Failed", err.Error()
			statuses = append(statuses, st)
			allReady, firstErr = false, err
			break
		}

		st.Phase = "Ready"
		st.Message = rel.Info.Description
		ts := metav1.Time{Time: rel.Info.FirstDeployed.Time}
		st.LastDeployed = &ts
		if rel.Info != nil {
			st.Revision = rel.Version
		}
		statuses = append(statuses, st)
	}

	// Carry over status of components not yet reached this pass.
	for _, name := range order[len(statuses):] {
		statuses = append(statuses, platformv1alpha1.ComponentStatus{Name: name, Phase: "Pending"})
	}
	stack.Status.Components = statuses

	if allReady {
		stack.Status.Phase = "Ready"
		meta.SetStatusCondition(&stack.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionTrue, Reason: "AllComponentsReady",
			ObservedGeneration: stack.Generation,
		})
	} else {
		stack.Status.Phase = "Progressing"
		msg := "deployment in progress"
		if firstErr != nil {
			msg = firstErr.Error()
		}
		meta.SetStatusCondition(&stack.Status.Conditions, metav1.Condition{
			Type: "Ready", Status: metav1.ConditionFalse, Reason: "ComponentsNotReady",
			Message: msg, ObservedGeneration: stack.Generation,
		})
	}
	if err := r.Status().Update(ctx, &stack); err != nil {
		return ctrl.Result{}, err
	}

	if firstErr != nil {
		log.Error(firstErr, "stack deployment failed")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if !allReady {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Requeue periodically to catch chart drift / def updates.
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *StackReconciler) fail(ctx context.Context, stack *platformv1alpha1.Stack, reason string, err error) error {
	stack.Status.Phase = "Failed"
	meta.SetStatusCondition(&stack.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, Reason: reason,
		Message: err.Error(), ObservedGeneration: stack.Generation,
	})
	if uerr := r.Status().Update(ctx, stack); uerr != nil {
		return fmt.Errorf("%v (status update: %w)", err, uerr)
	}
	return err
}

func (r *StackReconciler) finalize(ctx context.Context, stack *platformv1alpha1.Stack) error {
	if !controllerutil.ContainsFinalizer(stack, stackFinalizer) {
		return nil
	}
	for _, comp := range stack.Status.Components {
		if err := r.Helm.Uninstall(comp.Name, stack.Namespace); err != nil {
			return err
		}
	}
	controllerutil.RemoveFinalizer(stack, stackFinalizer)
	return r.Update(ctx, stack)
}

// SetupWithManager sets up the controller with the Manager.
func (r *StackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.Stack{}).
		Named("stack").
		Complete(r)
}
