/*
Copyright 2025 The Kubo Authors.
*/

package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"helm.sh/helm/v3/pkg/chart"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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
type bundleCacheEntry struct {
	charts map[string]*chart.Chart
	images map[string]bundleImage
}

type StackReconciler struct {
	client.Client
	Scheme      *runtime.Scheme
	Helm        *HelmEngine
	Flux        *FluxStrategy
	bundleCache sync.Map // digest string → bundleCacheEntry
}

// +kubebuilder:rbac:groups=platform.kubo.io,resources=stacks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.kubo.io,resources=stacks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.kubo.io,resources=stacks/finalizers,verbs=update
// +kubebuilder:rbac:groups=platform.kubo.io,resources=stackdefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=platform.kubo.io,resources=stackreleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.kubo.io,resources=stackreleases/status,verbs=get;update;patch
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

	secretsReady, err := r.ensureSecrets(ctx, &stack)
	if err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.fail(ctx, &stack, "SecretSyncFailed", err)
	}
	if !secretsReady {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if err := r.ensureVirtualService(ctx, &stack); err != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.fail(ctx, &stack, "VirtualServiceFailed", err)
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
	for _, extra := range stack.Spec.ExtraBundles {
		ec, ei, err := r.chartsFromBundleSource(ctx, &stack, extra)
		if err != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, r.fail(ctx, &stack, "BundlePullFailed", err)
		}
		if bundleCharts == nil {
			bundleCharts = ec
			bundleImages = ei
		} else {
			// ponytail: add-only merge — extra bundles fill missing keys, never overwrite
			// main bundle charts. This prevents dai-bundle's "backend" chart from
			// shadowing the foundation bundle's "backend" chart.
			for k, v := range ec {
				if _, exists := bundleCharts[k]; !exists {
					bundleCharts[k] = v
				}
			}
			for k, v := range ei {
				if _, exists := bundleImages[k]; !exists {
					bundleImages[k] = v
				}
			}
		}
	}
	if stack.Spec.GitRef != nil {
		gitCharts, err := r.chartsFromGitRepository(ctx, &stack)
		if err != nil {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, r.fail(ctx, &stack, "GitPullFailed", err)
		}
		if bundleCharts == nil {
			bundleCharts = gitCharts
		} else {
			for k, v := range gitCharts {
				bundleCharts[k] = v
			}
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
		// An explicit chartRef.RepoURL means the component pins a specific chart
		// source — don't let a bundle chart with the same name shadow it.
		var ch *chart.Chart
		fromBundle := false
		if comp.ChartRef.RepoURL == "" {
			ch = bundleCharts[comp.ChartRef.ChartName]
			fromBundle = ch != nil
			if ch == nil {
				ch = bundleCharts[name]
				fromBundle = ch != nil
			}
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
				if firstErr == nil {
					firstErr = err
				}
				// Isolate: a chart-pull failure for one component must not
				// starve every component after it in the order.
				continue
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
			r.upsertStackRelease(ctx, &stack, st)
			continue
		}
		st.Phase = platformv1alpha1.ComponentPhaseReady
		st.Revision = rel.Version
		st.Message = rel.Info.Description
		statuses = append(statuses, st)
		r.upsertStackRelease(ctx, &stack, st)
	}
	if len(rest) > 0 {
		var more []platformv1alpha1.ComponentStatus
		switch mode {
		case platformv1alpha1.DeploymentModeFlux:
			more, firstErr = r.Flux.Reconcile(ctx, &stack, &def, rest, byName)
			requeue = time.Minute
		default:
			more, firstErr = r.deployDirect(ctx, &stack, rest, byName)
		}
		for _, st := range more {
			r.upsertStackRelease(ctx, &stack, st)
		}
		statuses = append(statuses, more...)
	}

	opStatus, opErr := r.reconcileOperators(ctx, &stack, bundleCharts)
	statuses = append(statuses, opStatus...)
	if opErr != nil && firstErr == nil {
		firstErr = opErr
	}

	// Re-run secret propagation after Helm deploys: charts may create secrets
	// with empty values (e.g. dai-frontend) that overwrite the operator's copy.
	// A second pass ensures operator-propagated data always wins.
	if _, serr := r.ensureSecrets(ctx, &stack); serr != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, r.fail(ctx, &stack, "SecretSyncFailed", serr)
	}

	allReady := true
	for _, st := range statuses {
		if st.Phase != platformv1alpha1.ComponentPhaseReady {
			allReady = false
		}
	}

	// Prune releases dropped from the stack (exclude/removed component). Compares
	// against the spec component set, not the attempted statuses, so a component
	// that fails mid-reconcile is not spuriously pruned.
	desired := map[string]bool{}
	for name := range byName {
		desired[name] = true
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

// deployDirect deploys all components via the embedded Helm engine.
// Components are grouped into dependency waves from the DAG; every component
// in a wave has all its dependencies in earlier waves, so a wave's members
// deploy concurrently (bounded by maxDeployWorkers). Failures are isolated:
// a failed component's dependents are skipped, but unrelated components in
// later waves still deploy.
func (r *StackReconciler) deployDirect(
	ctx context.Context,
	stack *platformv1alpha1.Stack,
	order []string,
	byName map[string]platformv1alpha1.StackComponentSpec,
) ([]platformv1alpha1.ComponentStatus, error) {
	waves, err := deployWaves(order, byName)
	if err != nil {
		return nil, err
	}

	byWave := map[string]int{} // name -> wave index
	for i, wave := range waves {
		for _, name := range wave {
			byWave[name] = i
		}
	}

	statuses := make([]platformv1alpha1.ComponentStatus, 0, len(order))
	failed := map[string]bool{}
	var firstErr error

	for _, wave := range waves {
		runnable := make([]platformv1alpha1.StackComponentSpec, 0, len(wave))
		for _, name := range wave {
			comp := byName[name]
			// Skip components whose dependencies failed this pass — the
			// deploy would fail anyway and the error would bury the real one.
			var brokenDep string
			for _, dep := range comp.DependsOn {
				if failed[dep] {
					brokenDep = dep
					break
				}
			}
			if brokenDep != "" {
				msg := fmt.Sprintf("skipped: dependency %q failed", brokenDep)
				statuses = append(statuses, platformv1alpha1.ComponentStatus{
					Name: name, Phase: platformv1alpha1.ComponentPhasePending, Message: msg,
				})
				failed[name] = true
				continue
			}
			runnable = append(runnable, comp)
		}
		if len(runnable) == 0 {
			continue
		}

		results := make([]platformv1alpha1.ComponentStatus, len(runnable))
		errs := make([]error, len(runnable))
		var wg sync.WaitGroup
		sem := make(chan struct{}, maxDeployWorkers)
		for i, comp := range runnable {
			wg.Add(1)
			go func(i int, comp platformv1alpha1.StackComponentSpec) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				results[i], errs[i] = r.deployComponent(ctx, stack, comp)
			}(i, comp)
		}
		wg.Wait()

		for i, comp := range runnable {
			st := results[i]
			statuses = append(statuses, st)
			if errs[i] != nil && firstErr == nil {
				firstErr = errs[i]
			}
			if st.Phase == platformv1alpha1.ComponentPhaseFailed {
				failed[comp.Name] = true
			}
		}
	}
	return statuses, firstErr
}

// maxDeployWorkers bounds how many components of one dependency wave are
// deployed concurrently.
const maxDeployWorkers = 4

// deployWaves groups a topologically ordered component list into waves: every
// component's dependencies live in strictly earlier waves, so each wave can
// be deployed concurrently.
func deployWaves(order []string, byName map[string]platformv1alpha1.StackComponentSpec) ([][]string, error) {
	level := map[string]int{}
	maxDep := func(name string) int {
		m := 0
		for _, dep := range byName[name].DependsOn {
			if level[dep] > m {
				m = level[dep]
			}
		}
		return m
	}
	var waves [][]string
	for _, name := range order { // order is topological
		lvl := maxDep(name) + 1
		level[name] = lvl
		for len(waves) <= lvl {
			waves = append(waves, nil)
		}
		waves[lvl] = append(waves[lvl], name)
	}
	// Components with no dependencies land at level 1; drop the empty wave 0
	// so roots deploy in the first pass.
	if len(waves) > 0 && len(waves[0]) == 0 {
		waves = waves[1:]
	}
	return waves, nil
}

// deployComponent deploys one component and returns its resulting status.
// The returned status always has Phase set; err is non-nil when deployment
// failed.
func (r *StackReconciler) deployComponent(
	ctx context.Context,
	stack *platformv1alpha1.Stack,
	comp platformv1alpha1.StackComponentSpec,
) (platformv1alpha1.ComponentStatus, error) {
	st := platformv1alpha1.ComponentStatus{Name: comp.Name, Phase: platformv1alpha1.ComponentPhaseDeploying}
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
		return st, err
	}

	values := resolveComponentValues(&comp.Values, &stack.Spec.Values, stack.Spec.ComponentValues, comp.Name)
	targetNS := stack.Namespace
	if isClusterComponent(comp) {
		targetNS = clusterOperatorsNamespace
		if adopted, done, aerr := r.adoptClusterRelease(comp.Name, targetNS); done || aerr != nil {
			if aerr != nil {
				adopted.Phase, adopted.Message = platformv1alpha1.ComponentPhaseFailed, aerr.Error()
				return adopted, aerr
			}
			return adopted, nil
		}
	}
	rel, err := r.Helm.Deploy(comp.Name, targetNS, chart, values)
	if err != nil {
		st.Phase, st.Message = platformv1alpha1.ComponentPhaseFailed, err.Error()
		return st, err
	}

	st.Phase = platformv1alpha1.ComponentPhaseReady
	st.Message = rel.Info.Description
	ts := metav1.Time{Time: rel.Info.FirstDeployed.Time}
	st.LastDeployed = &ts
	if rel.Info != nil {
		st.Revision = rel.Version
	}
	return st, nil
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

// ensureSecrets copies secrets from kubo-system into the Stack's namespace:
//   - chart/bundle pull secrets referenced in the spec
//   - explicit tenant secrets listed in spec.secretsRef
// ensureSecrets returns (allReady, error). allReady is false when any source
// secret was missing — the caller should requeue rather than proceed.
func (r *StackReconciler) ensureSecrets(ctx context.Context, stack *platformv1alpha1.Stack) (bool, error) {
	log := logf.FromContext(ctx)
	const srcNS = "kubo-system"

	// pull secrets: from → to (same name)
	pullNames := map[string]struct{}{}
	if stack.Spec.Bundle != nil && stack.Spec.Bundle.SecretRef != nil {
		pullNames[stack.Spec.Bundle.SecretRef.Name] = struct{}{}
	}
	for _, extra := range stack.Spec.ExtraBundles {
		if extra.SecretRef != nil && extra.SecretRef.Name != "" {
			pullNames[extra.SecretRef.Name] = struct{}{}
		}
	}
	if stack.Spec.Inline != nil {
		for _, comp := range stack.Spec.Inline.Components {
			if comp.ChartPullSecretRef != nil && comp.ChartPullSecretRef.Name != "" {
				pullNames[comp.ChartPullSecretRef.Name] = struct{}{}
			}
		}
	}
	type mapping struct{ from, to string }
	var mappings []mapping
	for name := range pullNames {
		mappings = append(mappings, mapping{name, name})
	}
	for _, m := range stack.Spec.SecretsRef {
		to := m.To
		if to == "" {
			to = m.From
		}
		mappings = append(mappings, mapping{m.From, to})
	}

	allReady := true
	for _, m := range mappings {
		var src corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Namespace: srcNS, Name: m.from}, &src); err != nil {
			if errors.IsNotFound(err) {
				log.Info("secret not found in kubo-system, will retry", "secret", m.from)
				allReady = false
				continue
			}
			return false, err
		}
		dst := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      m.to,
				Namespace: stack.Namespace,
				// Prevent Helm from resetting this secret on upgrade.
				Annotations: map[string]string{"helm.sh/resource-policy": "keep"},
			},
			Type: src.Type,
			Data: src.Data,
		}
		// Use Update if already exists so rotation propagates.
		var existing corev1.Secret
		err := r.Get(ctx, client.ObjectKey{Namespace: stack.Namespace, Name: m.to}, &existing)
		if errors.IsNotFound(err) {
			if cerr := r.Create(ctx, &dst); cerr != nil && !errors.IsAlreadyExists(cerr) {
				return false, fmt.Errorf("copy secret %s→%s: %w", m.from, m.to, cerr)
			}
			log.Info("copied secret", "from", m.from, "to", m.to, "namespace", stack.Namespace)
		} else if err == nil {
			existing.Data = src.Data
			existing.Type = src.Type
			if existing.Annotations == nil {
				existing.Annotations = map[string]string{}
			}
			existing.Annotations["helm.sh/resource-policy"] = "keep"
			if uerr := r.Update(ctx, &existing); uerr != nil {
				return false, fmt.Errorf("sync secret %s→%s: %w", m.from, m.to, uerr)
			}
		} else {
			return false, err
		}
	}
	return allReady, nil
}

// SetupWithManager sets up the controller with the Manager. In Flux mode,
// HelmRelease updates are mapped back to the owning Stack.
func (r *StackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.Stack{}).
		Named("stack").
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}).
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
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				if obj.GetNamespace() != "kubo-system" {
					return nil
				}
				secretName := obj.GetName()
				var stacks platformv1alpha1.StackList
				if err := r.List(ctx, &stacks); err != nil {
					return nil
				}
				var reqs []reconcile.Request
				for _, s := range stacks.Items {
					for _, ref := range s.Spec.SecretsRef {
						if ref.From == secretName {
							reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: s.Namespace, Name: s.Name}})
							break
						}
					}
				}
				return reqs
			}),
		).
		Complete(r)
}

// upsertStackRelease creates or updates a StackRelease for the given component status.
func (r *StackReconciler) upsertStackRelease(ctx context.Context, stack *platformv1alpha1.Stack, st platformv1alpha1.ComponentStatus) {
	name := stack.Name + "-" + st.Name
	sr := &platformv1alpha1.StackRelease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: stack.Namespace},
	}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, sr, func() error {
		sr.Spec = platformv1alpha1.StackReleaseSpec{StackRef: stack.Name, Component: st.Name}
		sr.Status.Phase = st.Phase
		sr.Status.Revision = st.Revision
		sr.Status.Message = st.Message
		if st.Phase == platformv1alpha1.ComponentPhaseReady {
			now := metav1.Now()
			sr.Status.LastDeployedAt = &now
		}
		return controllerutil.SetOwnerReference(stack, sr, r.Scheme)
	})
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to upsert StackRelease", "component", st.Name)
	}
}
