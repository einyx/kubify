package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

// Mirrored images — upstream images are Docker Hub hosted and rate-limited
// or unavailable from inside clusters.
const (
	vaultImage      = "hashicorp/vault:1.14.8"
	bankVaultsImage = "ghcr.io/bank-vaults/bank-vaults:v1.33.1"
	vaultSAName     = "vault"
	vaultUnsealKey  = "vault-unseal-keys"
	vaultRBACName   = "vault-secrets"
)

// reinitSealedVault recovers the deadlock where the Vault CR (and with it
// the unseal-keys secret) was deleted while the raft PVC survived: vault
// can never unseal again and every reconcile would wait forever. Tenant
// vaults are fully re-seedable from seedVault, so the stale raft is wiped
// and bank-vaults re-initializes on the next pass. Opt out with
// spec.seedVault.autoReinit: false for tenants that keep non-reseedable
// data in Vault. Returns true when a wipe was performed.
func (r *StackReconciler) reinitSealedVault(ctx context.Context, stack *platformv1alpha1.Stack) (bool, error) {
	if stack.Spec.SeedVault != nil && stack.Spec.SeedVault.AutoReinit != nil && !*stack.Spec.SeedVault.AutoReinit {
		return false, nil
	}
	ns := stack.Namespace

	// Deadlock signature: the raft PVC is older than the current Vault CR.
	// A PVC younger than (or equal to) the CR is a normal first init.
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(schema.GroupVersionKind{Group: "vault.banzaicloud.com", Version: "v1alpha1", Kind: "Vault"})
	cr.SetName("vault")
	cr.SetNamespace(ns)
	if err := r.Get(ctx, client.ObjectKeyFromObject(cr), cr); err != nil {
		return false, err
	}
	pvc := &corev1.PersistentVolumeClaim{}
	pvcName := "vault-file-vault-0" // bank-vaults: <cr>-file-<cr>-0
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: pvcName}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil // no raft yet — plain first init
		}
		return false, err
	}
	if !pvc.CreationTimestamp.Time.Before(cr.GetCreationTimestamp().Time) {
		return false, nil
	}

	logf.FromContext(ctx).Info("vault: stale raft detected (Vault CR recreated after init) — wiping for re-init; seedVault will restore contents",
		"pvc", pvcName, "pvcCreated", pvc.CreationTimestamp.Time, "crCreated", cr.GetCreationTimestamp().Time)
	// Pod first: a running pod keeps the PVC in Terminating.
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vault-0", Namespace: ns}}
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err := r.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	return true, nil
}

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
		// The unseal secret never appearing while a raft PVC predates the
		// Vault CR means the CR (and the keys) were deleted after init —
		// vault is sealed forever. Recover by wiping the stale raft so
		// bank-vaults re-initializes; seedVault restores the contents.
		if wiped, werr := r.reinitSealedVault(ctx, stack); werr != nil {
			return fmt.Errorf("vault reinit check: %w", werr)
		} else if wiped {
			return fmt.Errorf("vault re-initializing (stale raft wiped, waiting for %s secret)", vaultUnsealKey)
		}
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

	// Bootstrap first: create any missing kubo-system Secrets (generated
	// per-tenant credentials + copied static shared credentials) so ensure
	// -secrets propagation has real data for a fresh tenant.
	if len(seed.Static)+len(seed.Generated) > 0 {
		if err := r.ensureVaultSeedSecrets(ctx, seed); err != nil {
			return fmt.Errorf("bootstrap secrets: %w", err)
		}
	}

	if seed.SourceSecret == "" || len(seed.Entries) == 0 {
		return nil // nothing to mirror into Vault this pass
	}

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

// ensureVaultSeedSecrets creates missing kubo-system Secrets at first
// bootstrap: Static entries copy shared credentials from canonical Secrets,
// Generated entries create fresh random per-tenant values. Existing Secrets
// are never overwritten; Generated merges only still-missing keys.
func (r *StackReconciler) ensureVaultSeedSecrets(ctx context.Context, seed *platformv1alpha1.VaultSeed) error {
	const sysNS = "kubo-system"
	for _, list := range [][]platformv1alpha1.VaultSeedSecret{seed.Static, seed.Generated} {
		for _, bs := range list {
			var existing corev1.Secret
			err := r.Get(ctx, types.NamespacedName{Namespace: sysNS, Name: bs.Name}, &existing)
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			exists := err == nil

			// Merge semantics: keep existing values, fill only what is
			// missing. Nothing is ever overwritten.
			data := map[string][]byte{}
			secretType := bs.Type
			if exists {
				for k, v := range existing.Data {
					data[k] = v
				}
				secretType = existing.Type
			}

			for k, v := range bs.Literal {
				if _, ok := data[k]; !ok {
					data[k] = []byte(v)
				}
			}
			if bs.CopyFrom != nil {
				srcNS := bs.CopyFrom.Namespace
				if srcNS == "" {
					srcNS = sysNS
				}
				var src corev1.Secret
				if err := r.Get(ctx, types.NamespacedName{Namespace: srcNS, Name: bs.CopyFrom.Name}, &src); err != nil {
					if apierrors.IsNotFound(err) {
						return fmt.Errorf("bootstrap copyFrom source %s/%s not found", srcNS, bs.CopyFrom.Name)
					}
					return err
				}
				if len(bs.CopyFrom.Keys) == 0 {
					// Whole-secret copy: all keys, source type preserved
					// when the entry does not pin a type.
					for k, v := range src.Data {
						if _, ok := data[k]; !ok {
							data[k] = v
						}
					}
					if bs.Type == "" && !exists {
						secretType = src.Type
					}
				}
				for dst, srck := range bs.CopyFrom.Keys {
					if _, ok := data[dst]; !ok {
						v, ok := src.Data[srck]
						if !ok {
							return fmt.Errorf("bootstrap copyFrom %s/%s missing key %s", srcNS, bs.CopyFrom.Name, srck)
						}
						data[dst] = v
					}
				}
			}
			for k, g := range bs.Generate {
				if _, ok := data[k]; ok {
					continue // never overwrite an existing value
				}
				v, err := generateValue(g)
				if err != nil {
					return fmt.Errorf("generate %s/%s: %w", bs.Name, k, err)
				}
				if g.Kind == "tls" {
					// tls produces two keys out of one entry; drop the
					// complementary key if it is also declared.
					if k == "tls.key" {
						delete(data, "tls.crt")
					} else {
						delete(data, "tls.key")
					}
					data["tls.key"] = []byte(v.key)
					data["tls.crt"] = []byte(v.cert)
					continue
				}
				data[k] = []byte(v.data)
			}

			if secretType == "" {
				secretType = corev1.SecretTypeOpaque
			}
			desired := corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      bs.Name,
					Namespace: sysNS,
					Labels:    map[string]string{"app.kubernetes.io/managed-by": "kubo"},
				},
				Type: secretType,
				Data: data,
			}
			if exists {
				existing.Data = desired.Data
				existing.Type = desired.Type
				if uerr := r.Update(ctx, &existing); uerr != nil {
					return fmt.Errorf("update %s: %w", bs.Name, uerr)
				}
			} else if cerr := r.Create(ctx, &desired); cerr != nil && !apierrors.IsAlreadyExists(cerr) {
				return fmt.Errorf("create %s: %w", bs.Name, cerr)
			}
		}
	}
	return nil
}

type generatedValue struct {
	data string
	key  string
	cert string
}

// generateValue produces one random value per kind.
func generateValue(g platformv1alpha1.GeneratedKey) (generatedValue, error) {
	switch g.Kind {
	case "hex":
		n := g.Length
		if n == 0 {
			n = 32
		}
		b := make([]byte, (n+1)/2)
		if _, err := rand.Read(b); err != nil {
			return generatedValue{}, err
		}
		s := hex.EncodeToString(b)
		return generatedValue{data: s[:n]}, nil
	case "base64":
		n := g.Length
		if n == 0 {
			n = 32
		}
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			return generatedValue{}, err
		}
		return generatedValue{data: base64.StdEncoding.EncodeToString(b)}, nil
	case "uuid":
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return generatedValue{}, err
		}
		b[6] = (b[6] & 0x0f) | 0x40 // version 4
		b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
		s := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
		return generatedValue{data: s}, nil
	case "scheduler-credentials":
		password := make([]byte, 18)
		accessKey := make([]byte, 10)
		secretKey := make([]byte, 30)
		if _, err := rand.Read(password); err != nil {
			return generatedValue{}, err
		}
		if _, err := rand.Read(accessKey); err != nil {
			return generatedValue{}, err
		}
		if _, err := rand.Read(secretKey); err != nil {
			return generatedValue{}, err
		}
		doc := map[string]interface{}{
			"scheduler@kubify.io": map[string]interface{}{
				"password": base64.RawURLEncoding.EncodeToString(password),
				"is_admin": true,
				"username": "scheduler@kubify.io",
				"keypair": map[string]string{
					"access_key_id":     "AKIA" + strings.ToUpper(hex.EncodeToString(accessKey)),
					"secret_access_key": base64.RawURLEncoding.EncodeToString(secretKey),
				},
			},
		}
		b, err := json.Marshal(doc)
		if err != nil {
			return generatedValue{}, err
		}
		return generatedValue{data: string(b)}, nil
	case "bcrypt":
		pw := make([]byte, 24)
		if _, err := rand.Read(pw); err != nil {
			return generatedValue{}, err
		}
		hash, err := bcrypt.GenerateFromPassword(pw, bcrypt.DefaultCost)
		if err != nil {
			return generatedValue{}, err
		}
		return generatedValue{data: string(hash)}, nil
	case "tls":
		key, crt, err := generateSelfSignedCert()
		if err != nil {
			return generatedValue{}, err
		}
		return generatedValue{key: key, cert: crt}, nil
	default:
		return generatedValue{}, fmt.Errorf("unknown generate kind %q", g.Kind)
	}
}

// generateSelfSignedCert returns PEM-encoded RSA key and certificate.
func generateSelfSignedCert() (keyPEM, crtPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "kubo-tenant"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	keyBuf, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBuf}))
	crtPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return keyPEM, crtPEM, nil
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
	return vc.ensureKVNamed(ctx, "secret")
}

// ensureKVNamed enables a KV v2 mount at the given path if absent.
func (vc *vaultClient) ensureKVNamed(ctx context.Context, mount string) error {
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
	if m, ok := mounts.Data[mount+"/"]; ok && m.Type == "kv" {
		return nil
	}
	payload := fmt.Sprintf(`{"type":"kv","options":{"version":"2"}}`)
	resp2, err := vc.post(ctx, "/v1/sys/mounts/"+mount, []byte(payload))
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
					"storageClassName": "managed-csi",
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
