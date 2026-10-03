package controller

import (
	"context"
	"fmt"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

// clusterOperatorsNamespace is where shared operators are installed once.
// They watch every namespace, so each Stack keeps its own Vault and Spark jobs.
// Not a tenant namespace and not kubo-system. One copy watches every stack.
const clusterOperatorsNamespace = "operators"

// Fixed NodePorts for the shared istio-ingress gateway; the kind cluster maps
// host 80/443 to these node ports via extraPortMappings.
const (
	ingressHTTPNodePort  = 30080
	ingressHTTPSNodePort = 30443
)

// platformOperators is kubo's registry of platform infrastructure managed
// outside the per-component deploy loop, keyed by the spec.operators toggle
// that enables each entry. This is PLATFORM knowledge (infrastructure), not
// product knowledge: products are described entirely by templates, bundles
// and component values. Components and charts carrying one of these names
// are routed to the operators namespace / reconcileOperators instead of the
// tenant release loop.
var platformOperators = map[string]bool{
	"vault-operator":    true,
	"vault-tenant":      true,
	"spark-operator":    true,
	"istiod":            true,
	"istio-ingress":     true,
	"kafka-operator":    true,
	"kubegres":          true,
	"cert-manager":      true,
	"training-operator": true,
	"agentfw":           true,
}

func isPlatformOperator(name string) bool {
	return platformOperators[name]
}

func isClusterComponent(comp platformv1alpha1.StackComponentSpec) bool {
	if comp.Scope == platformv1alpha1.ComponentScopeCluster {
		return true
	}
	return isPlatformOperator(comp.Name) || isPlatformOperator(comp.ChartRef.ChartName)
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

func certManagerChart() platformv1alpha1.ChartRef {
	return platformv1alpha1.ChartRef{
		RepoURL:      "oci://quay.io/jetstack/charts",
		ChartName:    "cert-manager",
		ChartVersion: "v1.18.2",
	}
}

func kubegresChart() platformv1alpha1.ChartRef {
	return platformv1alpha1.ChartRef{
		RepoURL:      "https://www.kubegres.io/charts",
		ChartName:    "kubegres",
		ChartVersion: "1.6.2",
	}
}

func kubeflowTrainingOperatorChart() platformv1alpha1.ChartRef {
	return platformv1alpha1.ChartRef{
		RepoURL:      "oci://ghcr.io/kubeflow/charts",
		ChartName:    "training-operator",
		ChartVersion: "v1.8.1",
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
		{"vault-tenant", stack.Spec.Operators != nil && stack.Spec.Operators.Vault},
		{"spark-operator", stack.Spec.Operators != nil && stack.Spec.Operators.Spark},
		{"istiod", stack.Spec.Operators != nil && stack.Spec.Operators.Istio},
		{"istio-ingress", stack.Spec.Operators != nil && stack.Spec.Operators.Istio},
		{"kafka-operator", stack.Spec.Operators != nil && stack.Spec.Operators.Kafka},
		{"cert-manager", stack.Spec.Operators != nil && stack.Spec.Operators.CertManager},
		{"kubegres", stack.Spec.Operators != nil && stack.Spec.Operators.Postgres},
		{"agentfw", stack.Spec.Operators != nil && stack.Spec.Operators.AgentFW},
		{"training-operator", stack.Spec.Operators != nil && stack.Spec.Operators.Kubeflow},
	} {
		if !op.on {
			// Shared cluster operators are never auto-removed: they hold
			// state (Vault storage, gateway config, CRDs) that outlives any
			// one Stack. Removal is explicit: helm uninstall in the
			// operators/istio-ingress namespaces. Only per-tenant pieces
			// (vault-tenant, agentfw) are cleaned up, in finalize.
			continue
		}
		if op.name == "vault-operator" || op.name == "vault-tenant" || op.name == "agentfw" {
			st := platformv1alpha1.ComponentStatus{Name: op.name, Phase: platformv1alpha1.ComponentPhaseReady}
			var perr error
			switch op.name {
			case "vault-tenant":
				perr = r.ensureTenantVault(ctx, stack)
				st.Message = "tenant vault reconciled + seeded"
			case "agentfw":
				perr = r.ensureTenantAgentFW(ctx, stack)
				st.Message = "agent firewall reconciled"
			}
			if perr != nil {
				st.Phase = platformv1alpha1.ComponentPhaseFailed
				st.Message = perr.Error()
				if firstErr == nil {
					firstErr = perr
				}
			}
			statuses = append(statuses, st)
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
	if ch == nil && name == "cert-manager" {
		ch, err = r.Helm.EnsureChart(certManagerChart(), "", clusterOperatorsNamespace)
	}
	if ch == nil && name == "kubegres" {
		ch, err = r.Helm.EnsureChart(kubegresChart(), "", clusterOperatorsNamespace)
	}
	if ch == nil && name == "istio-ingress" {
		ch, err = r.Helm.EnsureChart(istioGatewayChart(), "", clusterOperatorsNamespace)
	}
	if ch == nil && name == "training-operator" {
		ch, err = r.Helm.EnsureChart(kubeflowTrainingOperatorChart(), "", clusterOperatorsNamespace)
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
	if name == "cert-manager" {
		values = map[string]interface{}{
			"crds": map[string]interface{}{"enabled": true},
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{"cpu": "50m", "memory": "64Mi"},
			},
		}
	}
	if name == "training-operator" {
		values = map[string]interface{}{
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{"cpu": "100m", "memory": "256Mi"},
			},
		}
	}
	rel, err := r.Helm.Deploy(name, ns, ch, values)
	if err != nil {
		st.Phase = platformv1alpha1.ComponentPhaseFailed
		st.Message = err.Error()
		return st, err
	}
	if name == "istio-ingress" {
		if perr := r.pinIngressNodePorts(ctx, ns); perr != nil {
			st.Message = "deployed; nodeport pinning pending: " + perr.Error()
			return st, nil
		}
	}
	st.Phase = platformv1alpha1.ComponentPhaseReady
	st.Revision = rel.Version
	st.Message = rel.Info.Description
	return st, nil
}

// ingressNodePorts pins the gateway Service to fixed NodePorts so the kind
// cluster's extraPortMappings (host 80 -> node 30080, host 443 -> 30443)
// reach it without a port-forward.
func (r *StackReconciler) pinIngressNodePorts(ctx context.Context, ns string) error {
	var svc corev1.Service
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: "istio-ingress"}, &svc); err != nil {
		return err
	}
	changed := false
	for i := range svc.Spec.Ports {
		switch svc.Spec.Ports[i].Port {
		case 80:
			if svc.Spec.Ports[i].NodePort != ingressHTTPNodePort {
				svc.Spec.Ports[i].NodePort = ingressHTTPNodePort
				changed = true
			}
		case 443:
			if svc.Spec.Ports[i].NodePort != ingressHTTPSNodePort {
				svc.Spec.Ports[i].NodePort = ingressHTTPSNodePort
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	return r.Update(ctx, &svc)
}

// releaseTenantResources removes only the per-tenant pieces a Stack owns
// (its Vault instance and agent firewall). Shared cluster operators are
// intentionally never uninstalled here — removal is explicit.
func (r *StackReconciler) releaseOperators(ctx context.Context, stack *platformv1alpha1.Stack) error {
	if err := r.releaseOperator(ctx, stack, "vault-tenant"); err != nil {
		return err
	}
	return r.releaseOperator(ctx, stack, "agentfw")
}

func (r *StackReconciler) releaseOperator(ctx context.Context, stack *platformv1alpha1.Stack, name string) error {
	switch name {
	case "vault-tenant":
		return r.deleteTenantVault(ctx, stack)
	case "agentfw":
		return r.deleteAgentFW(ctx, stack)
	}
	// Shared operators are never uninstalled by stack lifecycle.
	return nil
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
