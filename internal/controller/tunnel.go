package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kptr "k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"


	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

// Pinned connector image. Bump deliberately; mirror into the tenant registry
// for clusters without Docker Hub egress.
const tunnelImage = "cloudflare/cloudflared:2025.1.0"

// ensureTunnel reconciles the per-tenant cloudflared connector Deployment.
// With a nil spec the connector is removed (the owner reference also GCs it
// when the Stack is deleted). Hostname→service routing is configured in
// Cloudflare; see docs/design/ingress-tunnel.md.
func (r *StackReconciler) ensureTunnel(ctx context.Context, stack *platformv1alpha1.Stack) error {
	name := stack.Name + "-tunnel"
	ns := stack.Namespace

	if stack.Spec.Tunnel == nil {
		var dep appsv1.Deployment
		err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &dep)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if dep.Labels["app.kubernetes.io/managed-by"] == "kubo" {
			return r.Delete(ctx, &dep)
		}
		return nil
	}

	t := stack.Spec.Tunnel
	token := t.TokenSecret
	if token == nil {
		// Convention: the token is seeded via VaultSeed into
		// "<stack>-tunnel-token", key "token".
		token = &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: stack.Name + "-tunnel-token"},
			Key:                  "token",
		}
	}
	if token.Name == "" {
		return fmt.Errorf("tunnel.tokenSecret.name is required")
	}
	if token.Key == "" {
		token.Key = "token"
	}

	labels := map[string]string{
		"app.kubernetes.io/managed-by": "kubo",
		"app.kubernetes.io/name":       "cloudflared",
		"app.kubernetes.io/instance":   name,
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
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, dep, func() error {
		dep.Labels = labels
		dep.Annotations = map[string]string{
			"platform.kubo.io/tunnel-hostname": t.Hostname,
		}
		dep.Spec = tunnelDeploymentSpec(token, name, labels)
		return controllerutil.SetControllerReference(stack, dep, r.Scheme)
	})
	return err
}

func tunnelDeploymentSpec(token *corev1.SecretKeySelector, name string, labels map[string]string) appsv1.DeploymentSpec {
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
					Args:  []string{"tunnel", "--no-autoupdate", "run"},
					Env: []corev1.EnvVar{{
						Name: "TUNNEL_TOKEN",
						ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: token.Name},
							Key:                  token.Key,
						}},
					}},
					SecurityContext: &corev1.SecurityContext{
						RunAsNonRoot:             kptr.To(true),
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
			},
		},
	}
}
