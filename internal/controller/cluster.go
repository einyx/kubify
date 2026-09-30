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
	case "vault-operator", "spark-operator", "istiod", "istio-ingress", "kafka-operator", "kubegres":
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
	st := platformv1alpha1.ComponentStatus{Name: name, Phase: platformv1alpha1.ComponentPhaseDeploying, Scope: platformv1alpha1.ComponentScopeCluster}
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

func kafkaOperatorChart() platformv1alpha1.ChartRef {
	return platformv1alpha1.ChartRef{
		RepoURL:      "https://strimzi.io/charts",
		ChartName:    "strimzi-kafka-operator",
		ChartVersion: "0.45.0",
	}
}

func kubegresChart() platformv1alpha1.ChartRef {
	return platformv1alpha1.ChartRef{
		RepoURL:      "https://www.kubegres.io/charts",
		ChartName:    "kubegres",
		ChartVersion: "1.6.2",
	}
}

func istioGatewayChart() platformv1alpha1.ChartRef {
	return platformv1alpha1.ChartRef{
		RepoURL:      "https://blob.istio.io/istio-release/charts",
		ChartName:    "gateway",
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
		{"istio-ingress", stack.Spec.Operators != nil && stack.Spec.Operators.Istio},
		{"kafka-operator", stack.Spec.Operators != nil && stack.Spec.Operators.Kafka},
		{"kubegres", stack.Spec.Operators != nil && stack.Spec.Operators.Postgres},
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
	ns := clusterOperatorsNamespace
	if name == "istio-ingress" {
		ns = "istio-ingress"
	}
	if adopted, done, err := r.adoptClusterRelease(name, ns); done || err != nil {
		if err != nil {
			adopted.Phase = platformv1alpha1.ComponentPhaseFailed
			adopted.Message = err.Error()
		}
		return adopted, err
	}
	ch := bundleCharts[name]
	var err error
	if ch == nil && name == "spark-operator" {
		ch, err = r.Helm.EnsureChart(sparkOperatorChart(), "", clusterOperatorsNamespace)
	}
	if ch == nil && name == "istiod" {
		ch, err = r.Helm.EnsureChart(istiodChart(), "", clusterOperatorsNamespace)
	}
	if ch == nil && name == "kafka-operator" {
		ch, err = r.Helm.EnsureChart(kafkaOperatorChart(), "", clusterOperatorsNamespace)
	}
	if ch == nil && name == "kubegres" {
		ch, err = r.Helm.EnsureChart(kubegresChart(), "", clusterOperatorsNamespace)
	}
	if ch == nil && name == "istio-ingress" {
		ch, err = r.Helm.EnsureChart(istioGatewayChart(), "", clusterOperatorsNamespace)
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
	if name == "kafka-operator" {
		values = map[string]interface{}{
			"watchAllNamespaces": true,
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{"cpu": "100m", "memory": "128Mi"},
			},
		}
	}
	if name == "kubegres" {
		values = map[string]interface{}{
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{"cpu": "50m", "memory": "64Mi"},
			},
		}
	}
	if name == "istio-ingress" {
		values = map[string]interface{}{
			"labels": map[string]interface{}{"istio": "ingressgateway"},
		}
	}
	rel, err := r.Helm.Deploy(name, ns, ch, values)
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
	for _, name := range []string{"vault-operator", "spark-operator", "istiod", "istio-ingress", "kafka-operator", "kubegres"} {
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

// clusterReleaseInUse reports whether another live Stack has a cluster-scoped
// component with the given name installed in the operators namespace. Fails
// safe: on a listing error the release is treated as in use.
func (r *StackReconciler) clusterReleaseInUse(ctx context.Context, stack *platformv1alpha1.Stack, name string) bool {
	var list platformv1alpha1.StackList
	if err := r.List(ctx, &list); err != nil {
		return true
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Namespace == stack.Namespace && other.Name == stack.Name {
			continue
		}
		if other.DeletionTimestamp != nil {
			continue
		}
		for _, comp := range other.Status.Components {
			if comp.Name == name && comp.Scope == platformv1alpha1.ComponentScopeCluster {
				return true
			}
		}
	}
	return false
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
		if name == "kafka-operator" && other.Spec.Operators.Kafka {
			return true
		}
		if name == "kubegres" && other.Spec.Operators.Postgres {
			return true
		}
		if name == "istio-ingress" && other.Spec.Operators.Istio {
			return true
		}
	}
	return false
}
