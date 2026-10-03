package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kptr "k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

// Pinned connector image. Bump deliberately; mirror into the tenant registry
// for clusters without Docker Hub egress.
const tunnelImage = "cloudflare/cloudflared:2025.1.0"

// ensureTunnel reconciles the per-tenant cloudflared connector Deployment,
// its credentials Secret mount, and the rendered tunnel config (ingress:
// hostname → http://frontend:80, 404 catch-all). Locally-managed mode: no
// manual Cloudflare dashboard steps. With a nil spec everything is removed
// (owner references also GC it when the Stack is deleted). See
// docs/design/ingress-tunnel.md.
func (r *StackReconciler) ensureTunnel(ctx context.Context, stack *platformv1alpha1.Stack) error {
	name := stack.Name + "-tunnel"
	ns := stack.Namespace

	if stack.Spec.Tunnel == nil {
		for _, obj := range []client.Object{
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		} {
			err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			if obj.GetLabels()["app.kubernetes.io/managed-by"] == "kubo" {
				if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
					return err
				}
			}
		}
		return nil
	}

	t := stack.Spec.Tunnel
	if t.TunnelID == "" {
		return fmt.Errorf("tunnel.tunnelID is required")
	}
	creds := t.CredentialsSecret
	if creds == nil {
		creds = &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: stack.Name + "-tunnel-credentials"},
			Key:                  "credentials.json",
		}
	}
	if creds.Name == "" {
		return fmt.Errorf("tunnel.credentialsSecret.name is required")
	}
	if creds.Key == "" {
		creds.Key = "credentials.json"
	}

	labels := map[string]string{
		"app.kubernetes.io/managed-by": "kubo",
		"app.kubernetes.io/name":       "cloudflared",
		"app.kubernetes.io/instance":   name,
	}

	// Rendered tunnel config: locally-managed mode, so hostname→service
	// routing is declarative here instead of a Cloudflare dashboard step.
	config := fmt.Sprintf(`tunnel: %s
credentials-file: /etc/cloudflared/credentials.json
ingress:
  - hostname: %s
    service: http://frontend:80
  - service: http_status:404
`, t.TunnelID, t.Hostname)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Data:       map[string]string{"config.yaml": config},
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
			Annotations: map[string]string{
				"platform.kubo.io/tunnel-hostname": t.Hostname,
			},
		},
	}
	depSpec := tunnelDeploymentSpec(t, creds, name, labels)
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = labels
		dep.Annotations = map[string]string{
			"platform.kubo.io/tunnel-hostname": t.Hostname,
		}
		dep.Spec = depSpec
		return controllerutil.SetControllerReference(stack, dep, r.Scheme)
	})
	if err != nil {
		return err
	}

	// ConfigMap is owned by the Deployment's controller reference pattern:
	// create-or-update with the same labels; owner reference GCs it.
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = labels
		cm.Data = map[string]string{"config.yaml": config}
		return controllerutil.SetControllerReference(stack, cm, r.Scheme)
	})
	return err
}

func tunnelDeploymentSpec(t *platformv1alpha1.StackTunnel, creds *corev1.SecretKeySelector, name string, labels map[string]string) appsv1.DeploymentSpec {
	replicas := int32(1)
	return appsv1.DeploymentSpec{
		Replicas: &replicas,
		Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  "cloudflared",
					Image: tunnelImage,
					Args: []string{
						"tunnel",
						"--no-autoupdate",
						"--config", "/etc/cloudflared/config.yaml",
						"run",
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "credentials", MountPath: "/etc/cloudflared/credentials.json", SubPath: creds.Key, ReadOnly: true},
						{Name: "config", MountPath: "/etc/cloudflared/config.yaml", SubPath: "config.yaml", ReadOnly: true},
					},
					SecurityContext: &corev1.SecurityContext{
						RunAsNonRoot: kptr.To(true),
						// The cloudflared image declares the user as
						// "nonroot" (non-numeric), which the kubelet cannot
						// verify against runAsNonRoot — pin the UID.
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
							SecretName: creds.Name,
							Items:      []corev1.KeyToPath{{Key: creds.Key, Path: "credentials.json"}},
						}},
					},
					{
						Name: "config",
						VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: name},
							Items:                []corev1.KeyToPath{{Key: "config.yaml", Path: "config.yaml"}},
						}},
					},
				},
			},
		},
	}
}
