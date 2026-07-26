package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

// Mirrored images — upstream images are Docker Hub hosted and rate-limited
// or unavailable from inside clusters.
const (
	vaultImage      = "ghcr.io/einyx/vault:1.14.8"
	bankVaultsImage = "ghcr.io/einyx/bank-vaults:1.16.3"
	vaultSAName     = "vault"
	vaultUnsealKey  = "vault-unseal-keys"
	vaultRBACName   = "vault-secrets"
)

// ensureTenantVault creates and maintains the per-tenant bank-vaults Vault
// instance: ServiceAccount (with pull secret), RBAC for the unseal-keys
// secret, and the Vault CR itself. Everything is applied every reconcile so
// drift self-heals.
func (r *StackReconciler) ensureTenantVault(ctx context.Context, stack *platformv1alpha1.Stack) error {
	ns := stack.Namespace
	pullSecret := ""
	if stack.Spec.Bundle != nil && stack.Spec.Bundle.SecretRef != nil {
		pullSecret = stack.Spec.Bundle.SecretRef.Name
	}

	if err := applyVaultObject(ctx, r.Client, vaultServiceAccount(ns, pullSecret)); err != nil {
		return fmt.Errorf("vault serviceaccount: %w", err)
	}
	if err := applyVaultObject(ctx, r.Client, vaultSecretRole(ns)); err != nil {
		return fmt.Errorf("vault role: %w", err)
	}
	if err := applyVaultObject(ctx, r.Client, vaultSecretRoleBinding(ns)); err != nil {
		return fmt.Errorf("vault rolebinding: %w", err)
	}
	if err := applyVaultCR(ctx, r.Client, stack.Namespace, vaultCRSpec(stack.Namespace)); err != nil {
		return fmt.Errorf("vault CR: %w", err)
	}

	// Ready once bank-vaults has initialized Vault (unseal-keys secret exists).
	var sec corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: vaultUnsealKey}, &sec)
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("vault initializing (waiting for %s secret)", vaultUnsealKey)
	}
	if err != nil {
		return err
	}

	// Integrated seeding: mirror keys from the source Secret into Vault KV.
	// Runs in-cluster against vault.<ns>.svc — no CLI, no port-forward.
	if stack.Spec.SeedVault != nil {
		return r.seedTenantVault(ctx, stack)
	}
	return nil
}

// seedTenantVault copies entries from a plain k8s Secret into the tenant
// Vault KV store. Values live only in the source Secret and in Vault.
func (r *StackReconciler) seedTenantVault(ctx context.Context, stack *platformv1alpha1.Stack) error {
	ns := stack.Namespace
	seed := stack.Spec.SeedVault

	var src corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: seed.SourceSecret}, &src); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("waiting for seed source secret %s", seed.SourceSecret)
		}
		return err
	}

	var unseal corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: vaultUnsealKey}, &unseal); err != nil {
		return err
	}
	token := string(unseal.Data["vault-root"])
	if token == "" {
		return fmt.Errorf("%s has no vault-root key", vaultUnsealKey)
	}

	addr := fmt.Sprintf("http://vault.%s.svc.cluster.local:8200", ns)
	vc := &vaultClient{addr: addr, token: token}

	// Ensure the KV v2 mount exists (bank-vaults does not render
	// externalConfig.secrets on all operator versions).
	if err := vc.ensureKV(ctx); err != nil {
		return fmt.Errorf("kv mount: %w", err)
	}

	for _, e := range seed.Entries {
		data := map[string]interface{}{}
		for _, k := range e.Keys {
			v, ok := src.Data[k]
			if !ok {
				return fmt.Errorf("seed secret %s missing key %s", seed.SourceSecret, k)
			}
			data[k] = string(v)
		}
		payload, _ := json.Marshal(map[string]interface{}{"data": data})
		resp, err := vc.post(ctx, "/v1/secret/data/"+e.Path, payload)
		if err != nil {
			return fmt.Errorf("seed %s: %w", e.Path, err)
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
			return fmt.Errorf("seed %s: vault returned %d: %s", e.Path, resp.StatusCode, body)
		}
	}
	return nil
}

type vaultClient struct {
	addr  string
	token string
}

func (vc *vaultClient) post(ctx context.Context, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vc.addr+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", vc.token)
	req.Header.Set("Content-Type", "application/json")
	return (&http.Client{Timeout: 10 * time.Second}).Do(req)
}

func (vc *vaultClient) ensureKV(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, vc.addr+"/v1/sys/mounts", nil)
	req.Header.Set("X-Vault-Token", vc.token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var mounts struct {
		Data map[string]struct {
			Type string `json:"type"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&mounts); err != nil {
		return err
	}
	if m, ok := mounts.Data["secret/"]; ok && m.Type == "kv" {
		return nil
	}
	resp2, err := vc.post(ctx, "/v1/sys/mounts/secret", []byte(`{"type":"kv","options":{"version":"2"}}`))
	if err != nil {
		return err
	}
	defer resp2.Body.Close()
	if resp2.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp2.Body, 200))
		return fmt.Errorf("enable kv: %d: %s", resp2.StatusCode, body)
	}
	return nil
}

func vaultServiceAccount(ns, pullSecret string) *corev1.ServiceAccount {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: vaultSAName, Namespace: ns},
	}
	if pullSecret != "" {
		sa.ImagePullSecrets = []corev1.LocalObjectReference{{Name: pullSecret}}
	}
	return sa
}

func vaultSecretRole(ns string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: vaultRBACName, Namespace: ns},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"secrets"},
			Verbs:     []string{"create", "get", "update", "patch", "list"},
		}},
	}
}

func vaultSecretRoleBinding(ns string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: vaultRBACName, Namespace: ns},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     vaultRBACName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      "ServiceAccount",
			Name:      vaultSAName,
			Namespace: ns,
		}},
	}
}

func vaultCRSpec(ns string) map[string]interface{} {
	return map[string]interface{}{
		"size":            1,
		"image":           vaultImage,
		"bankVaultsImage": bankVaultsImage,
		"serviceAccount":  vaultSAName,
		"statsdDisabled":  true,
		"serviceType":     "ClusterIP",
		"volumeClaimTemplates": []interface{}{
			map[string]interface{}{
				"metadata": map[string]interface{}{"name": "vault-file"},
				"spec": map[string]interface{}{
					"accessModes":      []interface{}{"ReadWriteOnce"},
					"storageClassName": "standard",
					"resources": map[string]interface{}{
						"requests": map[string]interface{}{"storage": "1Gi"},
					},
				},
			},
		},
		"volumeMounts": []interface{}{
			map[string]interface{}{
				"name":      "vault-file",
				"mountPath": "/vault/file",
			},
		},
		"unsealConfig": map[string]interface{}{
			"kubernetes": map[string]interface{}{"secretNamespace": ns},
		},
		"config": map[string]interface{}{
			"storage": map[string]interface{}{
				"raft": map[string]interface{}{"path": "/vault/file", "cluster_name": "vault"},
			},
			"cluster_addr": fmt.Sprintf("http://vault-0.vault-internal.%s.svc.cluster.local:8200", ns),
			"api_addr":     fmt.Sprintf("http://vault.%s.svc.cluster.local:8200", ns),
			"listener": map[string]interface{}{
				"tcp": map[string]interface{}{"address": "0.0.0.0:8200", "tls_disable": true},
			},
			"ui": false,
		},
		"externalConfig": map[string]interface{}{
			"secrets": []interface{}{map[string]interface{}{
				"path":    "secret",
				"type":    "kv",
				"options": map[string]interface{}{"version": "2"},
			}},
			"policies": []interface{}{map[string]interface{}{
				"name": "allow_secrets",
				"rules": `path "secret/*" {
  capabilities = ["create", "read", "update", "delete", "list"]
}`,
			}},
			"auth": []interface{}{map[string]interface{}{
				"type": "kubernetes",
				"roles": []interface{}{map[string]interface{}{
					"name":                             "default",
					"bound_service_account_names":      "*",
					"bound_service_account_namespaces": ns,
					"policies":                         "allow_secrets",
					"ttl":                              "1h",
				}},
			}},
		},
	}
}

func applyVaultObject(ctx context.Context, c client.Client, desired client.Object) error {
	key := client.ObjectKeyFromObject(desired)
	existing := desired.DeepCopyObject().(client.Object)
	err := c.Get(ctx, key, existing)
	if apierrors.IsNotFound(err) {
		return c.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	switch d := desired.(type) {
	case *corev1.ServiceAccount:
		e := existing.(*corev1.ServiceAccount)
		e.ImagePullSecrets = d.ImagePullSecrets
		return c.Update(ctx, e)
	case *rbacv1.Role:
		e := existing.(*rbacv1.Role)
		e.Rules = d.Rules
		return c.Update(ctx, e)
	case *rbacv1.RoleBinding:
		e := existing.(*rbacv1.RoleBinding)
		e.Subjects = d.Subjects
		return c.Update(ctx, e)
	}
	return fmt.Errorf("unsupported kind %T", desired)
}

// deleteTenantVault removes the per-tenant Vault CR, RBAC, and ServiceAccount.
func (r *StackReconciler) deleteTenantVault(ctx context.Context, stack *platformv1alpha1.Stack) error {
	ns := stack.Namespace
	gvk := schema.GroupVersionKind{Group: "vault.banzaicloud.com", Version: "v1alpha1", Kind: "Vault"}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName("vault")
	u.SetNamespace(ns)
	if err := r.Delete(ctx, u); client.IgnoreNotFound(err) != nil {
		return err
	}
	rb := &rbacv1.RoleBinding{}
	rb.Name, rb.Namespace = vaultRBACName, ns
	if err := r.Delete(ctx, rb); client.IgnoreNotFound(err) != nil {
		return err
	}
	role := &rbacv1.Role{}
	role.Name, role.Namespace = vaultRBACName, ns
	if err := r.Delete(ctx, role); client.IgnoreNotFound(err) != nil {
		return err
	}
	sa := &corev1.ServiceAccount{}
	sa.Name, sa.Namespace = vaultSAName, ns
	if err := r.Delete(ctx, sa); client.IgnoreNotFound(err) != nil {
		return err
	}
	// ponytail: unseal-keys holds root/unseal key material — wipe on disable
	sec := &corev1.Secret{}
	sec.Name, sec.Namespace = vaultUnsealKey, ns
	return client.IgnoreNotFound(r.Delete(ctx, sec))
}

// applyVaultCR creates or updates the bank-vaults Vault CR via unstructured
// access (avoids importing the bank-vaults API package).
func applyVaultCR(ctx context.Context, c client.Client, ns string, spec map[string]interface{}) error {
	gvk := schema.GroupVersionKind{Group: "vault.banzaicloud.com", Version: "v1alpha1", Kind: "Vault"}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetName("vault")
	u.SetNamespace(ns)
	err := c.Get(ctx, client.ObjectKeyFromObject(u), u)
	if apierrors.IsNotFound(err) {
		u.Object["spec"] = spec
		return c.Create(ctx, u)
	}
	if err != nil {
		return err
	}
	cur, err := json.Marshal(u.Object["spec"])
	if err != nil {
		return err
	}
	want, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	if string(cur) == string(want) {
		return nil
	}
	u.Object["spec"] = spec
	return c.Update(ctx, u)
}
