package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strings"
	"time"
)

// Cloudflare DNS management: when a Cloudflare API token is present
// (kubo-system/kubo-cloudflare-api, key "token"), the operator ensures a
// proxied CNAME for every virtual host of a Stack, pointing at the managed
// cloudflared tunnel. Opt-in: no secret, no DNS management.
//
// Safety: the operator only touches records it created — every record is
// tagged with comment "managed-by:kubo stack=<ns>/<name>" and records
// without that tag are never modified or deleted.

var (
	cfAPISecret     = "kubo-cloudflare-api-token"
	cfAPITokenKey   = "token"
	cfAPIBase       = "https://api.cloudflare.com/client/v4" // var: tests point this at a stub
	cfRecordComment = "managed-by:kubo"                      // + " stack=<ns>/<name>" appended
	cfTunnelTarget  = ".cfargotunnel.com"
)

type cfDNSClient struct {
	token string
	hc    *http.Client
}

func newCFDNSClient(ctx context.Context, c client.Client) (*cfDNSClient, error) {
	var sec corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: sourceNamespace, Name: cfAPISecret}, &sec); err != nil {
		return nil, err // NotFound → caller treats as disabled
	}
	token := string(sec.Data[cfAPITokenKey])
	if token == "" {
		return nil, fmt.Errorf("secret %s/%s has no %q key", sourceNamespace, cfAPISecret, cfAPITokenKey)
	}
	return &cfDNSClient{token: token, hc: &http.Client{Timeout: 15 * time.Second}}, nil
}

// get performs a Cloudflare API GET and decodes the "result" field.
func (c *cfDNSClient) get(ctx context.Context, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfAPIBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	return c.do(req, out)
}

func (c *cfDNSClient) send(ctx context.Context, method, path string, body interface{}, out interface{}) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, cfAPIBase+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *cfDNSClient) do(req *http.Request, out interface{}) error {
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var envelope struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&envelope); err != nil {
		if resp.StatusCode >= 300 {
			return fmt.Errorf("cloudflare %s: %d", req.URL.Path, resp.StatusCode)
		}
		return err
	}
	if !envelope.Success {
		msg := "unknown error"
		if len(envelope.Errors) > 0 {
			msg = envelope.Errors[0].Message
		}
		return fmt.Errorf("cloudflare %s: %d: %s", req.URL.Path, resp.StatusCode, msg)
	}
	if out != nil && len(envelope.Result) > 0 {
		return json.Unmarshal(envelope.Result, out)
	}
	return nil
}

// zoneIDFor resolves the zone id whose name is the longest suffix match of
// the hostname (e.g. host foundation-a.example.com matches zone
// example.com, not a shorter/other zone).
func (c *cfDNSClient) zoneIDFor(ctx context.Context, host string) (string, error) {
	var zones []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := c.get(ctx, "/zones?per_page=50", &zones); err != nil {
		return "", err
	}
	best := ""
	for _, z := range zones {
		if host == z.Name || strings.HasSuffix(host, "."+z.Name) {
			if len(z.Name) > len(best) {
				best = z.Name
				// remember id via closure over zones below
				for _, zz := range zones {
					if zz.Name == z.Name {
						_ = zz
					}
				}
			}
		}
	}
	if best == "" {
		return "", fmt.Errorf("no Cloudflare zone matches %q", host)
	}
	for _, z := range zones {
		if z.Name == best {
			return z.ID, nil
		}
	}
	return "", fmt.Errorf("zone %q not found", best)
}

// ensureDNSRecord creates or heals the proxied CNAME for host → target.
// Returns the action taken: "created", "updated", "noop".
func (c *cfDNSClient) ensureDNSRecord(ctx context.Context, zoneID, host, target, comment string) (string, error) {
	var records []struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Name    string `json:"name"`
		Content string `json:"content"`
		Proxied bool   `json:"proxied"`
		Comment string `json:"comment"`
	}
	if err := c.get(ctx, fmt.Sprintf("/zones/%s/dns_records?type=CNAME&name=%s", zoneID, host), &records); err != nil {
		return "", err
	}
	want := map[string]interface{}{
		"type": "CNAME", "name": host, "content": target, "proxied": true, "comment": comment,
	}
	if len(records) == 0 {
		if err := c.send(ctx, http.MethodPost, "/zones/"+zoneID+"/dns_records", want, nil); err != nil {
			return "", err
		}
		return "created", nil
	}
	rec := records[0]
	if rec.Content == target && rec.Proxied && strings.HasPrefix(rec.Comment, "managed-by:kubo") {
		return "noop", nil
	}
	// Heal target/proxied/comment on records we own (tagged); untagged
	// records are left alone and reported.
	if !strings.HasPrefix(rec.Comment, "managed-by:kubo") && rec.Comment != "" {
		return "", fmt.Errorf("dns record %s exists but is not kubo-managed (comment %q) — resolve manually", host, rec.Comment)
	}
	if err := c.send(ctx, http.MethodPut, "/zones/"+zoneID+"/dns_records/"+rec.ID, want, nil); err != nil {
		return "", err
	}
	return "updated", nil
}

// deleteDNSRecord removes a record only if it carries the kubo tag.
func (c *cfDNSClient) deleteDNSRecord(ctx context.Context, zoneID, host string) error {
	var records []struct {
		ID      string `json:"id"`
		Comment string `json:"comment"`
	}
	if err := c.get(ctx, fmt.Sprintf("/zones/%s/dns_records?type=CNAME&name=%s", zoneID, host), &records); err != nil {
		return err
	}
	for _, rec := range records {
		if !strings.HasPrefix(rec.Comment, "managed-by:kubo") {
			continue // human-managed record: leave it
		}
		if err := c.send(ctx, http.MethodDelete, "/zones/"+zoneID+"/dns_records/"+rec.ID, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// ensureTenantDNS is the Stack-reconcile entry point: derives the virtual
// hosts from spec.virtualService and ensures a proxied CNAME per host,
// pointing at the managed cloudflared tunnel. Disabled (no-op) when the
// Cloudflare API token secret is absent.
func (r *StackReconciler) ensureTenantDNS(ctx context.Context, stack *platformv1alpha1.Stack) error {
	vs := stack.Spec.VirtualService
	if vs == nil {
		return nil
	}
	cfc, err := newCFDNSClient(ctx, r.Client)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil // token secret absent: DNS management disabled
		}
		return err // surface real errors (RBAC etc.)
	}
	tunnelID, err := r.tunnelID(ctx)
	if err != nil {
		return err
	}
	target := tunnelID + cfTunnelTarget

	zoneID, err := cfc.zoneIDFor(ctx, vs.Host)
	if err != nil {
		// Host suffix may belong to a zone not in this CF account: loud, but
		// non-fatal (the stack still reconciles).
		return fmt.Errorf("dns zone resolve for %s: %w", vs.Host, err)
	}
	comment := cfRecordComment + " stack=" + stack.Namespace + "/" + stack.Name
	for _, h := range append([]string{vs.Host}, vs.AdditionalHosts...) {
		action, err := cfc.ensureDNSRecord(ctx, zoneID, h, target, comment)
		if err != nil {
			return fmt.Errorf("dns %s for %s: %w", action, h, err)
		}
	}
	return nil
}

// deleteTenantDNS removes the kubo-tagged DNS records for a stack's hosts
// during finalization. Untagged records are never touched.
func (r *StackReconciler) deleteTenantDNS(ctx context.Context, stack *platformv1alpha1.Stack) error {
	vs := stack.Spec.VirtualService
	if vs == nil {
		return nil
	}
	cfc, err := newCFDNSClient(ctx, r.Client)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil
		}
		return err
	}
	zoneID, err := cfc.zoneIDFor(ctx, vs.Host)
	if err != nil {
		return err
	}
	for _, h := range append([]string{vs.Host}, vs.AdditionalHosts...) {
		if err := cfc.deleteDNSRecord(ctx, zoneID, h); err != nil {
			return err
		}
	}
	return nil
}

// tunnelID reads the cloudflared tunnel id from the managed tunnel config.
func (r *StackReconciler) tunnelID(ctx context.Context) (string, error) {
	var cm corev1.ConfigMap
	if err := r.Get(ctx, types.NamespacedName{Namespace: sourceNamespace, Name: cloudflaredConfigName}, &cm); err != nil {
		return "", err
	}
	for _, line := range strings.Split(cm.Data["config.yaml"], "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "tunnel:") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "tunnel:")), nil
		}
	}
	return "", fmt.Errorf("tunnel id not found in %s", cloudflaredConfigName)
}

// deleteTenantDNS removes the kubo-tagged DNS records for a stack's hosts
// during finalization. Untagged records are never touched.
