package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// The control-plane vault is kubo's own secret store: a single-node Vault in
// a dedicated namespace, unsealed via Azure Key Vault (keys live outside the
// cluster, so nothing is lost when kubo-system is wiped). It is the durable
// source-of-truth for kubo-system seed secrets:
//
//   - every resolved kubo-system secret is mirrored into KV at kubo-system/<name>
//   - a missing kubo-system source is restored from the vault before falling
//     back to external provisioning
//
// Tenant copies are never a source here; the vault is.
const (
	cpVaultNS   = "kubo-vault"
	cpVaultName = "kubo"
	cpVaultAddr = "http://vault.kubo-vault.svc.cluster.local:8200"
	cpKVPath    = "kubo-system"
	cpAzureKV   = "az-vel-data-demo-uaen-kv"
)

// ensureControlPlaneVault makes sure the kubo-owned Vault exists and is
// unsealed. It returns a ready client, or cpReady=false when the vault is
// still initializing (callers proceed without it and retry next reconcile).
func (r *StackReconciler) ensureControlPlaneVault(ctx context.Context) (*vaultClient, bool) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cpVaultNS}}
	if err := r.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, false
	}

	// Same identity + RBAC shape as tenant vaults. The ACR pull secret is
	// provisioned in kubo-vault (it lives outside kubo-system on purpose).
	pull := "acr-pull-secret"
	if err := applyVaultObject(ctx, r.Client, vaultServiceAccount(cpVaultNS, pull)); err != nil {
		return nil, false
	}
	if err := applyVaultObject(ctx, r.Client, vaultSecretRole(cpVaultNS)); err != nil {
		return nil, false
	}
	if err := applyVaultObject(ctx, r.Client, vaultSecretRoleBinding(cpVaultNS)); err != nil {
		return nil, false
	}

	if err := applyVaultCR(ctx, r.Client, cpVaultNS, controlPlaneVaultSpec()); err != nil {
		return nil, false
	}

	// bank-vaults initializes (Azure-unseals) and stores the root token in
	// the unseal-keys secret of the vault's own namespace — a namespace no
	// kubo-system wipe touches.
	var unseal corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: cpVaultNS, Name: vaultUnsealKey}, &unseal); err != nil {
		return nil, false
	}
	token := string(unseal.Data["vault-root"])
	if token == "" {
		return nil, false
	}

	vc := &vaultClient{addr: cpVaultAddr, token: token}
	if err := vc.ensureKVNamed(ctx, cpKVPath); err != nil {
		return nil, false
	}
	return vc, true
}

// controlPlaneVaultSpec: single-node raft vault, unsealed via a k8s secret
// in the kubo-vault namespace (bank-vaults generates the keys at init).
// kubo-vault is deliberately NOT kubo-system: it survives kubo-system
// wipes, which is the failure mode this vault defends against.
func controlPlaneVaultSpec() map[string]interface{} {
	spec := vaultCRSpec(cpVaultNS)
	return spec
}

// sourceNamespace is the kubo-owned namespace holding seed source secrets.
const sourceNamespace = "kubo-system"

// vaultRestore returns the kubo-system Secret stored in the control-plane
// vault at kubo-system/<name>, if any.
func vaultRestore(ctx context.Context, vc *vaultClient, name string) (*corev1.Secret, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		vc.addr+"/v1/"+cpKVPath+"/data/"+name, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("X-Vault-Token", vc.token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var body struct {
		Data struct {
			Data map[string]string `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, false
	}
	if len(body.Data.Data) == 0 {
		return nil, false
	}
	data := map[string][]byte{}
	for k, v := range body.Data.Data {
		data[k] = []byte(v)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cpSourceNS()},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}, true
}

// vaultMirror pushes a resolved kubo-system secret into the control-plane
// vault. Best-effort: a vault hiccup must never stall secret resolution —
// the caller logs the outcome.
func vaultMirror(ctx context.Context, vc *vaultClient, name string, src *corev1.Secret) error {
	data := map[string]string{}
	for k, v := range src.Data {
		data[k] = string(v)
	}
	payload, _ := json.Marshal(map[string]interface{}{"data": data})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		vc.addr+"/v1/"+cpKVPath+"/data/"+name, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("X-Vault-Token", vc.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("vault mirror %s: %d: %s", name, resp.StatusCode, body)
	}
	return nil
}

func cpSourceNS() string { return sourceNamespace }
