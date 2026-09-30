/*
Copyright 2025 The Kubo Authors.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"helm.sh/helm/v3/pkg/chart"
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
	Flux   *FluxStrategy
}

// +kubebuilder:rbac:groups=platform.kubo.io,resources=stacks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.kubo.io,resources=stacks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.kubo.io,resources=stacks/finalizers,verbs=update
// +kubebuilder:rbac:groups=platform.kubo.io,resources=stackdefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts;services;configmaps;persistentvolumeclaims;pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets;replicasets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies;ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vault.banzaicloud.com,resources=vaults,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmrepositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=helm.toolkit.fluxcd.io,resources=helmreleases/status,verbs=get

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

	var bundleCharts map[string]*chart.Chart
	var bundleImages map[string]bundleImage
	if stack.Spec.Bundle != nil && stack.Spec.Bundle.URL != "" {
		var err error
		bundleCharts, bundleImages, err = r.chartsFromBundle(ctx, &stack)
		if err != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, r.fail(ctx, &stack, "BundlePullFailed", err)
		}
	}

	// Resolve the stack definition from StackRef or Inline.
	var def platformv1alpha1.StackDefinition
	if stack.Spec.Inline != nil {
		def.Spec = *stack.Spec.Inline
	} else if stack.Spec.StackRef != "" {
		if err := r.Get(ctx, client.ObjectKey{Name: stack.Spec.StackRef}, &def); err != nil {
			meta.SetStatusCondition(&stack.Status.Conditions, metav1.Condition{
				Type: "Ready", Status: metav1.ConditionFalse,
				Reason: "StackDefinitionNotFound", Message: err.Error(),
			})
			stack.Status.Phase = "Failed"
			_ = r.Status().Update(ctx, &stack)
			return ctrl.Result{RequeueAfter: time.Minute}, client.IgnoreNotFound(err)
		}
	} else {
		excluded := map[string]bool{}
		for _, n := range stack.Spec.Exclude {
			excluded[n] = true
		}
		seen := map[*chart.Chart]string{}
		for name, ch := range bundleCharts {
			if excluded[name] || isClusterOperator(name) {
				continue
			}
			if _, ok := seen[ch]; ok {
				continue
			}
			seen[ch] = name
			def.Spec.Components = append(def.Spec.Components, platformv1alpha1.StackComponentSpec{
				Name:     name,
				ChartRef: platformv1alpha1.ChartRef{ChartName: name},
			})
		}
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

	mode := stack.Spec.Mode
	if mode == "" {
		mode = platformv1alpha1.DeploymentModeDirect
	}

	var statuses []platformv1alpha1.ComponentStatus
	var firstErr error
	requeue := 5 * time.Minute

	var rest []string
	for _, name := range order {
		comp := byName[name]
		if isClusterOperator(name) || isClusterOperator(comp.ChartRef.ChartName) {
			continue
		}
		ch := bundleCharts[comp.ChartRef.ChartName]
		fromBundle := ch != nil
		if ch == nil {
			ch = bundleCharts[name]
			fromBundle = ch != nil
		}
		if ch == nil && stack.Spec.Bundle != nil {
			var err error
			pullSecret := ""
			if stack.Spec.Bundle.SecretRef != nil {
				pullSecret = stack.Spec.Bundle.SecretRef.Name
			}
			ch, err = r.Helm.EnsureChart(comp.ChartRef, pullSecret, stack.Namespace)
			if err != nil {
				st := platformv1alpha1.ComponentStatus{Name: name, Phase: platformv1alpha1.ComponentPhaseFailed, Message: err.Error()}
				statuses = append(statuses, st)
				firstErr = err
				break
			}
		}
		if ch == nil {
			rest = append(rest, name)
			continue
		}
		st := platformv1alpha1.ComponentStatus{Name: name, Phase: platformv1alpha1.ComponentPhaseDeploying}
		if isClusterComponent(comp) {
			st.Scope = platformv1alpha1.ComponentScopeCluster
		}
		values := resolveComponentValues(&comp.Values, &stack.Spec.Values, stack.Spec.ComponentValues, name)
		if ch.Values != nil {
			values = mergeValues(ch.Values, values)
		}
		pullSecret := ""
		if stack.Spec.Bundle != nil && stack.Spec.Bundle.SecretRef != nil {
			pullSecret = stack.Spec.Bundle.SecretRef.Name
		}
		if fromBundle {
			rewriteBundleValues(values, bundleImages, pullSecret)
			if img, ok := matchBundleImage(name, bundleImages); ok {
				applyBundleImage(values, img, pullSecret)
			} else {
				logf.FromContext(ctx).Info("no bundle image for component; using chart defaults", "component", name)
			}
		}
		targetNS := stack.Namespace
		if isClusterComponent(comp) {
			targetNS = clusterOperatorsNamespace
			delete(values, "imagePullSecrets")
			values["watchNamespace"] = ""
			if st, done, err := r.adoptClusterRelease(name, targetNS); done || err != nil {
				if err != nil {
					st.Phase, st.Message = platformv1alpha1.ComponentPhaseFailed, err.Error()
					if firstErr == nil {
						firstErr = err
					}
				}
				statuses = append(statuses, st)
				continue
			}
		}
		rel, err := r.Helm.Deploy(name, targetNS, ch, values)
		if err != nil {
			st.Phase, st.Message = platformv1alpha1.ComponentPhaseFailed, err.Error()
			statuses = append(statuses, st)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		st.Phase = platformv1alpha1.ComponentPhaseReady
		st.Revision = rel.Version
		st.Message = rel.Info.Description
		statuses = append(statuses, st)
	}
	if firstErr == nil && len(rest) > 0 {
		var more []platformv1alpha1.ComponentStatus
		switch mode {
		case platformv1alpha1.DeploymentModeFlux:
			more, firstErr = r.Flux.Reconcile(ctx, &stack, &def, rest, byName)
			requeue = time.Minute
		default:
			more, firstErr = r.deployDirect(ctx, &stack, rest, byName)
		}
		statuses = append(statuses, more...)
	}

	opStatus, opErr := r.reconcileOperators(ctx, &stack, bundleCharts)
	statuses = append(statuses, opStatus...)
	if opErr != nil && firstErr == nil {
		firstErr = opErr
	}

	allReady := true
	for _, st := range statuses {
		if st.Phase != platformv1alpha1.ComponentPhaseReady {
			allReady = false
		}
	}

	// Prune releases dropped from the stack (exclude/removed component). Compares
	// against the last observed component set, so nothing outside this Stack is touched.
	desired := map[string]bool{}
	for _, s := range statuses {
		desired[s.Name] = true
	}
	for _, prev := range stack.Status.Components {
		if desired[prev.Name] {
			continue
		}
		if err := r.uninstallComponent(ctx, &stack, prev); err != nil {
			log.Info("prune uninstall failed", "component", prev.Name, "err", err.Error())
		} else {
			log.Info("pruned component", "component", prev.Name)
		}
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
		// ponytail: requeue on conflict instead of retrying inline; next reconcile re-reads fresh version
		return ctrl.Result{Requeue: true}, client.IgnoreNotFound(err)
	}

	if firstErr != nil {
		log.Error(firstErr, "stack deployment failed")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if !allReady {
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}

	// Requeue periodically to catch chart drift / def updates.
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// deployDirect deploys all components via the embedded Helm engine,
// stopping at the first failure (components are dependency-ordered).
func (r *StackReconciler) deployDirect(
	ctx context.Context,
	stack *platformv1alpha1.Stack,
	order []string,
	byName map[string]platformv1alpha1.StackComponentSpec,
) ([]platformv1alpha1.ComponentStatus, error) {
	var statuses []platformv1alpha1.ComponentStatus

	for _, name := range order {
		comp := byName[name]
		st := platformv1alpha1.ComponentStatus{Name: name, Phase: platformv1alpha1.ComponentPhaseDeploying}
		if isClusterComponent(comp) {
			st.Scope = platformv1alpha1.ComponentScopeCluster
		}

		pullSecret := ""
		if comp.ChartPullSecretRef != nil && comp.ChartPullSecretRef.Name != "" {
			pullSecret = comp.ChartPullSecretRef.Name
		} else if stack.Spec.Bundle != nil && stack.Spec.Bundle.SecretRef != nil {
			pullSecret = stack.Spec.Bundle.SecretRef.Name
		}
		chart, err := r.Helm.EnsureChart(comp.ChartRef, pullSecret, stack.Namespace)
		if err != nil {
			st.Phase, st.Message = platformv1alpha1.ComponentPhaseFailed, err.Error()
			statuses = append(statuses, st)
			return statuses, err // dependency order: stop at first failure
		}

		values := resolveComponentValues(&comp.Values, &stack.Spec.Values, stack.Spec.ComponentValues, name)
		targetNS := stack.Namespace
		if isClusterComponent(comp) {
			targetNS = clusterOperatorsNamespace
			if adopted, done, aerr := r.adoptClusterRelease(name, targetNS); done || aerr != nil {
				if aerr != nil {
					adopted.Phase, adopted.Message = platformv1alpha1.ComponentPhaseFailed, aerr.Error()
				}
				statuses = append(statuses, adopted)
				if aerr != nil {
					return statuses, aerr
				}
				continue
			}
		}
		rel, err := r.Helm.Deploy(name, targetNS, chart, values)
		if err != nil {
			st.Phase, st.Message = platformv1alpha1.ComponentPhaseFailed, err.Error()
			statuses = append(statuses, st)
			return statuses, err
		}

		st.Phase = platformv1alpha1.ComponentPhaseReady
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
		statuses = append(statuses, platformv1alpha1.ComponentStatus{Name: name, Phase: platformv1alpha1.ComponentPhasePending})
	}
	return statuses, nil
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
	mode := stack.Spec.Mode
	if mode == "" {
		mode = platformv1alpha1.DeploymentModeDirect
	}
	switch mode {
	case platformv1alpha1.DeploymentModeFlux:
		if r.Flux != nil {
			if err := r.Flux.Cleanup(ctx, stack); err != nil {
				return err
			}
		}
	default:
		for _, comp := range stack.Status.Components {
			if isClusterOperator(comp.Name) {
				continue // handled below via releaseOperators (refcounted)
			}
			if err := r.uninstallComponent(ctx, stack, comp); err != nil {
				return err
			}
		}
		if err := r.releaseOperators(ctx, stack); err != nil {
			return err
		}
	}
	controllerutil.RemoveFinalizer(stack, stackFinalizer)
	return r.Update(ctx, stack)
}

// uninstallComponent removes one component release from its namespace.
// Cluster-scoped releases live in the operators namespace and are only
// uninstalled when no other live Stack still uses them.
func (r *StackReconciler) uninstallComponent(ctx context.Context, stack *platformv1alpha1.Stack, comp platformv1alpha1.ComponentStatus) error {
	if comp.Scope == platformv1alpha1.ComponentScopeCluster {
		if isClusterOperator(comp.Name) {
			return nil
		}
		if r.clusterReleaseInUse(ctx, stack, comp.Name) {
			return nil
		}
		return r.Helm.Uninstall(comp.Name, clusterOperatorsNamespace)
	}
	return r.Helm.Uninstall(comp.Name, stack.Namespace)
}

// SetupWithManager sets up the controller with the Manager. In Flux mode,
// HelmRelease updates are mapped back to the owning Stack.
func (r *StackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.Stack{}).
		Named("stack").
		Watches(
			&helmv2.HelmRelease{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				owner := metav1.GetControllerOf(obj)
				if owner == nil || owner.Kind != "Stack" {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: owner.Name}}}
			}),
		).
		Complete(r)
}
