package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kptr "k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Cluster-level Cloudflare Tunnel: platform ingress infrastructure, not a
// tenant concern. Declared entirely by two kubo-system objects:
//
//   - ConfigMap kubo-cloudflared-config  (config.yaml: ingress rules,
//     typically tenant hostnames → istio-ingressgateway)
//   - Secret     kubo-cloudflared-credentials (credentials.json from
//     `cloudflared tunnel create`; provisioned via the control-plane vault)
//
// When the ConfigMap exists, the operator ensures the connector Deployment
// in kubo-system. When it is absent (or removed), the connector is torn
// down. See docs/design/ingress-tunnel.md.
const (
	cloudflaredConfigName = "kubo-cloudflared-config"
	cloudflaredCredsName  = "kubo-cloudflared-credentials"
	cloudflaredDeployName = "kubo-cloudflared"
	cloudflaredImage      = "cloudflare/cloudflared:2025.1.0"
	// The connector routes tenant hostnames to the shared gateway, which
	// routes by Host header to tenant VirtualServices — all product paths
	// (/ /dai /mcp /ai …) stay in the VirtualServices.
	gatewayServiceURL = "http://istio-ingressgateway.istio-system.svc.cluster.local"
)

// ensureCloudflareTunnel reconciles the cluster tunnel connector.
func (r *StackReconciler) ensureCloudflareTunnel(ctx context.Context) error {
	sysNS := "kubo-system"

	var cm corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Namespace: sysNS, Name: cloudflaredConfigName}, &cm)
	if apierrors.IsNotFound(err) {
		// No tunnel declared: tear the connector down.
		var dep appsv1.Deployment
		derr := r.Get(ctx, types.NamespacedName{Namespace: sysNS, Name: cloudflaredDeployName}, &dep)
		if apierrors.IsNotFound(derr) {
			return nil
		}
		if derr != nil {
			return derr
		}
		return r.Delete(ctx, &dep)
	}
	if err != nil {
		return err
	}
	config, ok := cm.Data["config.yaml"]
	if !ok || config == "" {
		return fmt.Errorf("%s has no config.yaml key", cloudflaredConfigName)
	}

	// The credentials Secret is provisioned out-of-band (control-plane
	// vault / VaultSeed). Missing credentials requeue via error.
	var creds corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: sysNS, Name: cloudflaredCredsName}, &creds); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("waiting for %s secret (cloudflared credentials.json)", cloudflaredCredsName)
		}
		return err
	}

	labels := map[string]string{
		"app.kubernetes.io/managed-by": "kubo",
		"app.kubernetes.io/name":       "cloudflared",
		"app.kubernetes.io/instance":   cloudflaredDeployName,
	}

	// Checksum the mounted inputs into the pod template: config.yaml and
	// credentials.json are subPath-mounted, and subPath volumes never
	// refresh in-place — without this, tunnel config edits and credential
	// rotations never reach the running cloudflared.
	sum := sha256.Sum256(append([]byte(config), creds.Data["credentials.json"]...))
	checksum := hex.EncodeToString(sum[:])
	podAnnotations := map[string]string{
		"kubo.io/tunnel-checksum": checksum,
	}

	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cloudflaredDeployName,
			Namespace: sysNS,
			Labels:    labels,
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = labels
		if dep.Spec.Template.ObjectMeta.Annotations == nil {
			dep.Spec.Template.ObjectMeta.Annotations = map[string]string{}
		}
		dep.Spec.Template.ObjectMeta.Annotations = podAnnotations
		dep.Spec = appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: podAnnotations},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "cloudflared",
						Image: cloudflaredImage,
						Args: []string{
							"tunnel",
							"--no-autoupdate",
							"--config", "/etc/cloudflared/config.yaml",
							"run",
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "credentials", MountPath: "/etc/cloudflared/credentials.json", SubPath: "credentials.json", ReadOnly: true},
							{Name: "config", MountPath: "/etc/cloudflared/config.yaml", SubPath: "config.yaml", ReadOnly: true},
						},
						SecurityContext: &corev1.SecurityContext{
							RunAsNonRoot: kptr.To(true),
							// cloudflared image user is non-numeric
							// ("nonroot") — pin the UID for runAsNonRoot.
							RunAsUser:                kptr.To(int64(65532)),
							ReadOnlyRootFilesystem:   kptr.To(true),
							AllowPrivilegeEscalation: kptr.To(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("200m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "credentials",
							VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
								SecretName: cloudflaredCredsName,
								Items:      []corev1.KeyToPath{{Key: "credentials.json", Path: "credentials.json"}},
							}},
						},
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: cloudflaredConfigName},
								Items:                []corev1.KeyToPath{{Key: "config.yaml", Path: "config.yaml"}},
							}},
						},
					},
				},
			},
		}
		return nil
	})
	return err
}
