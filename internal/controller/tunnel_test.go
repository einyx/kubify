package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

func TestTunnelDeploymentSpec(t *testing.T) {
	tun := &platformv1alpha1.StackTunnel{
		Hostname: "acme.customer.com",
		TunnelID: "938ce61b-362b-42b3-885c-d389b00fa5ab",
	}
	tok := corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "tunnel-token"}, Key: "credentials.json"}
	labels := map[string]string{"app.kubernetes.io/managed-by": "kubo"}
	spec := tunnelDeploymentSpec(tun, &tok, "product-x-tunnel", labels)

	c := spec.Template.Spec.Containers[0]
	if c.Image != tunnelImage {
		t.Errorf("image = %q", c.Image)
	}
	if c.Args[0] != "tunnel" || c.Args[2] != "run" {
		t.Errorf("args = %v", c.Args)
	}
	found := false
	for _, v := range c.VolumeMounts {
		if v.Name == "credentials" && v.MountPath != "/etc/cloudflared/credentials.json" {
			t.Errorf("credentials mount = %+v", v)
		}
		if v.Name == "config" {
			found = true
		}
	}
	if !found {
		t.Error("config volume mount missing")
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
	spec := tunnelDeploymentSpec(tun, &tok, "n", map[string]string{})
	if spec.Template.Spec.Containers[0].Env[0].ValueFrom.SecretKeyRef.Key != "token" {
		t.Error("key default not applied")
	}
}
