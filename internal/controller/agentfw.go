package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

const (
	agentfwName  = "agentfw"
	agentfwImage = "kubifyregistry.azurecr.io/kubo/agentfw:main" // ACR mirror — GHCR is rate-limited from clusters; sync via CI or `docker push`
	agentfwPort  = 8080
	// Admin endpoint: kill switch + session-archive viewer. The portal's
	// agentfw integration proxies this port (svc:<ns>/agentfw:8081).
	agentfwAdminPort = 8081
	agentfwPolicyKey = "policy.yaml"
)

// ensureTenantAgentFW reconciles the per-tenant agent firewall: ConfigMap, Deployment, Service.
func (r *StackReconciler) ensureTenantAgentFW(ctx context.Context, stack *platformv1alpha1.Stack) error {
	ns := stack.Namespace

	if err := r.ensureAgentFWConfigMap(ctx, ns); err != nil {
		return fmt.Errorf("agentfw configmap: %w", err)
	}
	if err := r.ensureAgentFWDeployment(ctx, ns); err != nil {
		return fmt.Errorf("agentfw deployment: %w", err)
	}
	if err := r.ensureAgentFWService(ctx, ns); err != nil {
		return fmt.Errorf("agentfw service: %w", err)
	}
	return nil
}

func (r *StackReconciler) ensureAgentFWConfigMap(ctx context.Context, ns string) error {
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: agentfwName, Namespace: ns},
		Data: map[string]string{
			agentfwPolicyKey: `allowedMCPTools: ["*"]
blockPrivateEgress: true
dlpAction: redact
injectionAction: block
# upstream: https://api.openai.com   # set to enable reverse-proxy mode
`,
		},
	}
	var existing corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: agentfwName}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Don't overwrite — user may have customised the policy
	return nil
}

func (r *StackReconciler) ensureAgentFWDeployment(ctx context.Context, ns string) error {
	replicas := int32(1)
	fsGroup := int64(65532)
	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: agentfwName, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": agentfwName}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": agentfwName}},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						// Let the nonroot agentfw (65532) write the viewer archive.
						FSGroup: &fsGroup,
					},
					Containers: []corev1.Container{{
						Name:  agentfwName,
						Image: agentfwImage,
						// :main is a mutable tracking tag — always re-pull so
						// nodes pick up fresh pushes instead of stale cache.
						ImagePullPolicy: corev1.PullAlways,
						Args:            []string{"-addr=:8080", "-admin=:8081", "-policy=/etc/agentfw/policy.yaml"},
						Ports: []corev1.ContainerPort{
							{ContainerPort: agentfwPort, Protocol: corev1.ProtocolTCP},
							{Name: "admin", ContainerPort: agentfwAdminPort, Protocol: corev1.ProtocolTCP},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "policy",
							MountPath: "/etc/agentfw",
							ReadOnly:  true,
						}, {
							// Viewer archive (SQLite) — /var/lib/agentfw must
							// exist and be writable by uid 65532.
							Name:      "viewer-archive",
							MountPath: "/var/lib/agentfw",
						}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("200m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "policy",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: agentfwName},
							},
						},
					}, {
						Name: "viewer-archive",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					}},
				},
			},
		},
	}

	var existing appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: agentfwName}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// Sync the pod-spec fields this controller owns. Containers alone is
	// not enough: a container that gains a volumeMount while Volumes stays
	// stale renders an invalid Deployment (volumeMounts[x].name not found)
	// that the API server rejects on every reconcile.
	existing.Spec.Template.Spec.Containers = desired.Spec.Template.Spec.Containers
	existing.Spec.Template.Spec.Volumes = desired.Spec.Template.Spec.Volumes
	existing.Spec.Template.Spec.SecurityContext = desired.Spec.Template.Spec.SecurityContext
	return r.Update(ctx, &existing)
}

func (r *StackReconciler) ensureAgentFWService(ctx context.Context, ns string) error {
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: agentfwName, Namespace: ns},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": agentfwName},
			Ports: []corev1.ServicePort{
				{
					// Multi-port Services require a name on every port.
					Name:       "proxy",
					Port:       agentfwPort,
					TargetPort: intstr.FromInt(agentfwPort),
					Protocol:   corev1.ProtocolTCP,
				},
				{
					Name:       "admin",
					Port:       agentfwAdminPort,
					TargetPort: intstr.FromInt(agentfwAdminPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
	var existing corev1.Service
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: agentfwName}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	// Self-heal drift (e.g. services created before the admin port existed).
	// Compare semantically: a same-count port mutation must also heal.
	if !equalPorts(existing.Spec.Ports, desired.Spec.Ports) {
		existing.Spec.Ports = desired.Spec.Ports
		return r.Update(ctx, &existing)
	}
	return err
}

func equalPorts(a, b []corev1.ServicePort) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Port != b[i].Port ||
			a[i].TargetPort != b[i].TargetPort || a[i].Protocol != b[i].Protocol {
			return false
		}
	}
	return true
}

// deleteAgentFW removes the per-tenant agent firewall resources.
func (r *StackReconciler) deleteAgentFW(ctx context.Context, stack *platformv1alpha1.Stack) error {
	ns := stack.Namespace
	for _, obj := range []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: agentfwName, Namespace: ns}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: agentfwName, Namespace: ns}},
		// ponytail: ConfigMap intentionally kept — preserves user policy customisations
	} {
		if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}
