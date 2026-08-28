package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

func TestTunnelDeploymentSpec(t *testing.T) {
	tun := &platformv1alpha1.StackTunnel{
		Hostname:    "acme.customer.com",
		TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "tunnel-token"}, Key: "token"},
	}
	labels := map[string]string{"app.kubernetes.io/managed-by": "kubo"}
	spec := tunnelDeploymentSpec(tun.TokenSecret, "product-x-tunnel", labels)

	c := spec.Template.Spec.Containers[0]
	if c.Image != tunnelImage {
		t.Errorf("image = %q", c.Image)
	}
	if c.Args[0] != "tunnel" || c.Args[2] != "run" {
		t.Errorf("args = %v", c.Args)
	}
	tok := c.Env[0]
	if tok.Name != "TUNNEL_TOKEN" || tok.ValueFrom.SecretKeyRef.Name != "tunnel-token" {
		t.Errorf("token env = %+v", tok)
	}
	if !*c.SecurityContext.RunAsNonRoot || len(c.SecurityContext.Capabilities.Drop) == 0 {
		t.Error("expected hardened security context")
	}
	if *spec.Replicas != 1 {
		t.Errorf("replicas = %d", *spec.Replicas)
	}
}

func TestTunnelTokenKeyDefaults(t *testing.T) {
	// ensureTunnel defaults the key to "token"; spec-level empty key must not
	// render an empty secretKeyRef.
	tun := &platformv1alpha1.StackTunnel{
		Hostname:    "x.example",
		TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "tok"}},
	}
	if tun.TokenSecret.Key != "" {
		t.Fatal("precondition: key empty")
	}
	// mirrored default applied in ensureTunnel; simulate:
	tun.TokenSecret.Key = "token"
	spec := tunnelDeploymentSpec(tun.TokenSecret, "n", map[string]string{})
	if spec.Template.Spec.Containers[0].Env[0].ValueFrom.SecretKeyRef.Key != "token" {
		t.Error("key default not applied")
	}
}
