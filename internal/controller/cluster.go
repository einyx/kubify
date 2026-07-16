package controller

import (
	"context"
	"fmt"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

// clusterOperatorsNamespace is where shared operators are installed once.
// They watch every namespace, so each Stack keeps its own Vault and Spark jobs.
// Not a tenant namespace and not kubo-system. One copy watches every stack.
const clusterOperatorsNamespace = "operators"

func isClusterOperator(name string) bool {
	switch name {
	case "vault-operator", "spark-operator", "istiod":
		return true
	default:
		return false
	}
}

func isClusterComponent(comp platformv1alpha1.StackComponentSpec) bool {
	if comp.Scope == platformv1alpha1.ComponentScopeCluster {
		return true
	}
	return isClusterOperator(comp.Name) || isClusterOperator(comp.ChartRef.ChartName)
}

// adoptClusterRelease reports an already-installed shared operator as Ready
// so a second Stack does not install another copy.
func (r *StackReconciler) adoptClusterRelease(name, namespace string) (platformv1alpha1.ComponentStatus, bool, error) {
	st := platformv1alpha1.ComponentStatus{Name: name, Phase: platformv1alpha1.ComponentPhaseDeploying}
	status, err := r.Helm.ReleaseStatus(name, namespace)
	if err != nil {
		return st, false, err
	}
	if status == release.StatusDeployed {
		st.Phase = platformv1alpha1.ComponentPhaseReady
		st.Message = "shared cluster operator already installed"
		return st, true, nil
	}
	return st, false, nil
}

func sparkOperatorChart() platformv1alpha1.ChartRef {
	return platformv1alpha1.ChartRef{
		RepoURL:      "https://kubeflow.github.io/spark-operator",
		ChartName:    "spark-operator",
		ChartVersion: "2.0.2",
	}
}

func istiodChart() platformv1alpha1.ChartRef {
	return platformv1alpha1.ChartRef{
		RepoURL:      "https://blob.istio.io/istio-release/charts",
		ChartName:    "istiod",
		ChartVersion: "1.24.1",
	}
}

func (r *StackReconciler) reconcileOperators(ctx context.Context, stack *platformv1alpha1.Stack, bundleCharts map[string]*chart.Chart) ([]platformv1alpha1.ComponentStatus, error) {
	var statuses []platformv1alpha1.ComponentStatus
	var firstErr error
	for _, op := range []struct {
		name string
		on   bool
	}{
		{"vault-operator", stack.Spec.Operators != nil && stack.Spec.Operators.Vault},
		{"spark-operator", stack.Spec.Operators != nil && stack.Spec.Operators.Spark},
		{"istiod", stack.Spec.Operators != nil && stack.Spec.Operators.Istio},
	} {
		if !op.on {
			if err := r.releaseOperator(ctx, stack, op.name); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		st, err := r.ensureOperator(ctx, op.name, bundleCharts)
		statuses = append(statuses, st)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return statuses, firstErr
}

func (r *StackReconciler) ensureOperator(ctx context.Context, name string, bundleCharts map[string]*chart.Chart) (platformv1alpha1.ComponentStatus, error) {
	st := platformv1alpha1.ComponentStatus{Name: name, Phase: platformv1alpha1.ComponentPhaseDeploying}
	if adopted, done, err := r.adoptClusterRelease(name, clusterOperatorsNamespace); done || err != nil {
		if err != nil {
			adopted.Phase = platformv1alpha1.ComponentPhaseFailed
			adopted.Message = err.Error()
		}
		return adopted, err
	}
	ch := bundleCharts[name]
	var err error
	if ch == nil && name == "spark-operator" {
		ch, err = r.Helm.EnsureChart(sparkOperatorChart())
	}
	if ch == nil && name == "istiod" {
		ch, err = r.Helm.EnsureChart(istiodChart())
	}
	if ch == nil && err == nil {
		err = fmt.Errorf("no chart for %s", name)
	}
	if err != nil {
		st.Phase = platformv1alpha1.ComponentPhaseFailed
		st.Message = err.Error()
		return st, err
	}
	values := map[string]interface{}{"watchNamespace": ""}
	if name == "spark-operator" {
		values = map[string]interface{}{
			"webhook": map[string]interface{}{"enable": false},
			"spark":   map[string]interface{}{"jobNamespaces": []interface{}{""}},
		}
	}
	if name == "istiod" {
		values = map[string]interface{}{
			"global": map[string]interface{}{"istioNamespace": clusterOperatorsNamespace},
			"pilot": map[string]interface{}{
				"autoscaleMin": 1,
				"resources": map[string]interface{}{
					"requests": map[string]interface{}{"cpu": "100m", "memory": "128Mi"},
				},
			},
		}
	}
	rel, err := r.Helm.Deploy(name, clusterOperatorsNamespace, ch, values)
	if err != nil {
		st.Phase = platformv1alpha1.ComponentPhaseFailed
		st.Message = err.Error()
		return st, err
	}
	st.Phase = platformv1alpha1.ComponentPhaseReady
	st.Revision = rel.Version
	st.Message = rel.Info.Description
	return st, nil
}

func (r *StackReconciler) releaseOperators(ctx context.Context, stack *platformv1alpha1.Stack) error {
	for _, name := range []string{"vault-operator", "spark-operator", "istiod"} {
		if err := r.releaseOperator(ctx, stack, name); err != nil {
			return err
		}
	}
	return nil
}

func (r *StackReconciler) releaseOperator(ctx context.Context, stack *platformv1alpha1.Stack, name string) error {
	if r.operatorWantedByOther(ctx, stack, name) {
		return nil
	}
	return r.Helm.Uninstall(name, clusterOperatorsNamespace)
}

func (r *StackReconciler) operatorWantedByOther(ctx context.Context, stack *platformv1alpha1.Stack, name string) bool {
	var list platformv1alpha1.StackList
	if err := r.List(ctx, &list); err != nil {
		return true
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Namespace == stack.Namespace && other.Name == stack.Name {
			continue
		}
		if other.DeletionTimestamp != nil || other.Spec.Operators == nil {
			continue
		}
		if name == "vault-operator" && other.Spec.Operators.Vault {
			return true
		}
		if name == "spark-operator" && other.Spec.Operators.Spark {
			return true
		}
		if name == "istiod" && other.Spec.Operators.Istio {
			return true
		}
	}
	return false
}
