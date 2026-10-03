// Package vaultkv is a minimal Vault KV v2 client used by the portal to
// manage per-tenant Vault secrets. Tokens are read from the tenant's
// vault-unseal-keys Secret and held in memory only — never logged or
// serialized into responses.
package vaultkv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	unsealSecret = "vault-unseal-keys"
	unsealKey    = "vault-root"
	defaultMount = "secret"
)

// Client talks to one Vault instance.
type Client struct {
	addr  string
	token string
	http  *http.Client
}

// NewWithAddr builds a Client with an explicit address and token (used by
// the portal for address overrides, e.g. local port-forwards or an API
// server proxy base address).
func NewWithAddr(addr, token string) *Client {
	return &Client{addr: addr, token: token, http: &http.Client{Timeout: 10 * time.Second}}
}

// WithHTTPClient replaces the HTTP client (portal: an authenticated client
// for Kubernetes API server proxy requests).
func (c *Client) WithHTTPClient(hc *http.Client) *Client {
	c.http = hc
	return c
}

// Token returns the current token (never expose it through APIs).
func (c *Client) Token() string { return c.token }

// Addr returns the Vault API base address.
func (c *Client) Addr() string { return c.addr }

// NewForNamespace builds a Client for the Vault in the given namespace using
// its vault-root token. The namespace must run a bank-vaults Vault with the
// standard vault-unseal-keys Secret.
func NewForNamespace(ctx context.Context, c client.Client, ns string) (*Client, error) {
	var unseal corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: unsealSecret}, &unseal); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("no Vault in namespace %q (%s secret missing)", ns, unsealSecret)
		}
		return nil, err
	}
	token := string(unseal.Data[unsealKey])
	if token == "" {
		return nil, fmt.Errorf("%s has no %s key", unsealSecret, unsealKey)
	}
	return &Client{
		addr:  fmt.Sprintf("http://vault.%s.svc.cluster.local:8200", ns),
		token: token,
		http:  &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// NewWithChildToken returns a Client that uses a short-lived child token
// instead of the root token. The child token is limited to policyName and
// expires after ttl. Use this for request-scoped work so long-lived root
// credentials never sit in per-request state.
func (c *Client) NewWithChildToken(ctx context.Context, policyName, ttl string) (*Client, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"policies":     []string{policyName},
		"ttl":          ttl,
		"renewable":    false,
		"no_parent":    true,
		"display_name": "kubo-portal",
	})
	var out struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/auth/token/create", body, &out); err != nil {
		return nil, err
	}
	if out.Auth.ClientToken == "" {
		return nil, fmt.Errorf("vault returned no client token")
	}
	return &Client{addr: c.addr, token: out.Auth.ClientToken, http: c.http}, nil
}

// Sealed reports whether the Vault is currently sealed.
func (c *Client) Sealed(ctx context.Context) (bool, error) {
	var st struct {
		Sealed bool `json:"sealed"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/sys/seal-status", nil, &st); err != nil {
		return false, err
	}
	return st.Sealed, nil
}

// Entry is one KV v2 secret with its version metadata.
type Entry struct {
	Path      string            `json:"path"`
	Data      map[string]string `json:"data"`
	Version   int               `json:"version"`
	CreatedAt string            `json:"createdAt"`
	Destroyed bool              `json:"destroyed"`
}

// List returns the raw metadata LIST keys under path (folders carry a
// trailing slash — use SplitList to separate them).
func (c *Client) List(ctx context.Context, path string) ([]string, error) {
	var out struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/"+defaultMount+"/metadata/"+path+"?list=true", nil, &out); err != nil {
		return nil, err
	}
	return out.Data.Keys, nil
}

// SplitList separates a metadata LIST result into leaf entries and folder
// prefixes (Vault marks folders with a trailing slash).
func SplitList(keys []string) (entries, folders []string) {
	for _, k := range keys {
		if strings.HasSuffix(k, "/") {
			folders = append(folders, k)
		} else {
			entries = append(entries, k)
		}
	}
	return
}

// Read fetches one entry's data and version metadata.
func (c *Client) Read(ctx context.Context, path string) (*Entry, error) {
	var out struct {
		Data struct {
			Data     map[string]interface{} `json:"data"`
			Metadata struct {
				Version     int    `json:"version"`
				CreatedTime string `json:"created_time"`
				Destroyed   bool   `json:"destroyed"`
			} `json:"metadata"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/"+defaultMount+"/data/"+path, nil, &out); err != nil {
		return nil, err
	}
	e := &Entry{Path: path, Version: out.Data.Metadata.Version, CreatedAt: out.Data.Metadata.CreatedTime, Destroyed: out.Data.Metadata.Destroyed}
	e.Data = map[string]string{}
	for k, v := range out.Data.Data {
		if s, ok := v.(string); ok {
			e.Data[k] = s
		} else {
			b, _ := json.Marshal(v)
			e.Data[k] = string(b)
		}
	}
	return e, nil
}

// Write creates a new version of an entry.
func (c *Client) Write(ctx context.Context, path string, data map[string]string) error {
	payload, _ := json.Marshal(map[string]interface{}{"data": data})
	return c.doStatus(ctx, http.MethodPost, "/v1/"+defaultMount+"/data/"+path, payload)
}

// Delete removes an entry: soft (a tombstone version) by default, or all
// versions when permanent.
func (c *Client) Delete(ctx context.Context, path string, permanent bool) error {
	if permanent {
		return c.doStatus(ctx, http.MethodDelete, "/v1/"+defaultMount+"/metadata/"+path, nil)
	}
	return c.doStatus(ctx, http.MethodDelete, "/v1/"+defaultMount+"/data/"+path, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, out interface{}) error {
	resp, err := c.roundTrip(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return statusErr(method, path, resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) doStatus(ctx context.Context, method, path string, body []byte) error {
	resp, err := c.roundTrip(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return statusErr(method, path, resp)
	}
	return nil
}

func (c *Client) roundTrip(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.addr+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", c.token)
	req.Header.Set("Content-Type", "application/json")
	return c.http.Do(req)
}

func statusErr(method, path string, resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	return fmt.Errorf("vault %s %s: %d: %s", method, path, resp.StatusCode, snippet)
}
