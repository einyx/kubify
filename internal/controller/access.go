package controller

import (
	"context"
	"fmt"
	"strings"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Cloudflare Access management: when the kubo-cloudflare-api-token secret
// carries an "account_id" key, the operator ensures a Cloudflare Access
// self_hosted application with an SSO allow policy for every virtual host
// of a Stack.
//
// Safety: the operator only touches Access apps whose name starts with
// "kubo:" — manually created apps are never modified or deleted.

var (
	cfAccessTag       = "kubo:"                            // name prefix for managed apps
	cfAccessSecretKey = "account_id"                       // extra key in kubo-cloudflare-api-token
	cfAccessIdPKey    = "access_idp_id"                    // SSO identity provider UUID
	cfAccessDomainKey = "access_allowed_email_domain"      // e.g. "meshx.io"
)

type cfAccessClient struct {
	*cfDNSClient
	accountID   string
	idpID       string
	emailDomain string
}

func newCFAccessClient(ctx context.Context, c client.Client) (*cfAccessClient, error) {
	var sec corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: sourceNamespace, Name: cfAPISecret}, &sec); err != nil {
		return nil, err
	}
	token := string(sec.Data[cfAPITokenKey])
	if token == "" {
		return nil, fmt.Errorf("secret %s/%s has no %q key", sourceNamespace, cfAPISecret, cfAPITokenKey)
	}
	accountID := strings.TrimSpace(string(sec.Data[cfAccessSecretKey]))
	if accountID == "" {
		return nil, fmt.Errorf("secret %s/%s has no %q key — Access management disabled", sourceNamespace, cfAPISecret, cfAccessSecretKey)
	}
	idpID := strings.TrimSpace(string(sec.Data[cfAccessIdPKey]))
	emailDomain := strings.TrimSpace(string(sec.Data[cfAccessDomainKey]))
	if emailDomain == "" {
		emailDomain = "meshx.io"
	}
	dns, _ := newCFDNSClient(ctx, c)
	return &cfAccessClient{
		cfDNSClient: dns,
		accountID:   accountID,
		idpID:       idpID,
		emailDomain: emailDomain,
	}, nil
}

type cfAccessApp struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Domain string `json:"domain"`
	Type   string `json:"type"`
}

type cfAccessPolicy struct {
	ID       string                   `json:"id"`
	Name     string                   `json:"name"`
	Decision string                   `json:"decision"`
	Include  []map[string]interface{} `json:"include"`
}

// appNameForHost returns the managed-app name for a given virtual host.
func appNameForHost(host string) string {
	return cfAccessTag + host
}

// appNameForStack returns the managed-app name for a given stack (primary host).
// Kept for backward-compat with apps created before per-host naming.
func appNameForStack(ns, name string) string {
	return cfAccessTag + ns + "/" + name
}

// listApps fetches all Access apps for the account once, so callers can
// avoid repeated API calls when processing multiple hosts.
func (ac *cfAccessClient) listApps(ctx context.Context) ([]cfAccessApp, error) {
	var apps []cfAccessApp
	if err := ac.get(ctx, fmt.Sprintf("/accounts/%s/access/apps", ac.accountID), &apps); err != nil {
		return nil, fmt.Errorf("list access apps: %w", err)
	}
	return apps, nil
}

// ensureAccessApp creates or updates a Cloudflare Access self_hosted
// application for the given host. The app is named by host so each virtual
// host (primary + additional) gets its own Access application.
func (ac *cfAccessClient) ensureAccessApp(ctx context.Context, apps []cfAccessApp, host string) (string, error) {
	appName := appNameForHost(host)

	for _, a := range apps {
		if a.Name == appName {
			if a.Domain == host {
				return a.ID, nil // already correct
			}
			// Domain drift — update.
			body := map[string]interface{}{
				"name":             appName,
				"domain":           host,
				"type":             "self_hosted",
				"session_duration": "24h",
			}
			if ac.idpID != "" {
				body["allowed_idps"] = []string{ac.idpID}
			}
			var updated cfAccessApp
			if err := ac.send(ctx, "PUT", fmt.Sprintf("/accounts/%s/access/apps/%s", ac.accountID, a.ID), body, &updated); err != nil {
				return "", fmt.Errorf("update access app %s: %w", appName, err)
			}
			return updated.ID, nil
		}
		// Migrate legacy apps named kubo:ns/name — update name to host-based.
	}

	// Create new app.
	body := map[string]interface{}{
		"name":             appName,
		"domain":           host,
		"type":             "self_hosted",
		"session_duration": "24h",
	}
	if ac.idpID != "" {
		body["allowed_idps"] = []string{ac.idpID}
	}
	var created cfAccessApp
	if err := ac.send(ctx, "POST", fmt.Sprintf("/accounts/%s/access/apps", ac.accountID), body, &created); err != nil {
		return "", fmt.Errorf("create access app %s: %w", appName, err)
	}

	// Attach the SSO allow policy.
	if err := ac.ensureAllowPolicy(ctx, created.ID); err != nil {
		return created.ID, fmt.Errorf("access policy for %s: %w", appName, err)
	}
	return created.ID, nil
}

// ensureAllowPolicy ensures an "allow meshx.io" policy exists on the app.
func (ac *cfAccessClient) ensureAllowPolicy(ctx context.Context, appID string) error {
	var policies []cfAccessPolicy
	if err := ac.get(ctx, fmt.Sprintf("/accounts/%s/access/apps/%s/policies", ac.accountID, appID), &policies); err != nil {
		return err
	}
	policyName := "kubo-sso"
	for _, p := range policies {
		if p.Name == policyName && p.Decision == "allow" {
			return nil // already exists
		}
	}
	include := []map[string]interface{}{
		{"email_domain": map[string]string{"domain": ac.emailDomain}},
	}
	if ac.idpID != "" {
		include = append(include, map[string]interface{}{
			"login_method": map[string]string{"id": ac.idpID},
		})
	}
	body := map[string]interface{}{
		"name":     policyName,
		"decision": "allow",
		"include":  include,
	}
	return ac.send(ctx, "POST", fmt.Sprintf("/accounts/%s/access/apps/%s/policies", ac.accountID, appID), body, nil)
}

// deleteAccessAppByHost removes the kubo-managed Access app for a host.
func (ac *cfAccessClient) deleteAccessAppByHost(ctx context.Context, apps []cfAccessApp, host string) error {
	appName := appNameForHost(host)
	for _, a := range apps {
		if a.Name == appName {
			return ac.send(ctx, "DELETE", fmt.Sprintf("/accounts/%s/access/apps/%s", ac.accountID, a.ID), nil, nil)
		}
	}
	return nil // already gone
}

// ensureTenantAccess is the Stack-reconcile entry point: ensures a
// Cloudflare Access app exists for every virtual host (primary +
// additionalHosts) with an SSO policy. Disabled (no-op) when the secret
// lacks an account_id key.
func (r *StackReconciler) ensureTenantAccess(ctx context.Context, stack *platformv1alpha1.Stack) error {
	vs := stack.Spec.VirtualService
	if vs == nil {
		return nil
	}
	ac, err := newCFAccessClient(ctx, r.Client)
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "disabled") {
			return nil
		}
		return err
	}
	apps, err := ac.listApps(ctx)
	if err != nil {
		return err
	}
	hosts := append([]string{vs.Host}, vs.AdditionalHosts...)
	for _, host := range hosts {
		if host == "" {
			continue
		}
		if _, err := ac.ensureAccessApp(ctx, apps, host); err != nil {
			return err
		}
	}
	return nil
}

// deleteTenantAccess removes the kubo-managed Access apps for all virtual
// hosts during finalization. Unmanaged apps are never touched.
func (r *StackReconciler) deleteTenantAccess(ctx context.Context, stack *platformv1alpha1.Stack) error {
	vs := stack.Spec.VirtualService
	if vs == nil {
		return nil
	}
	ac, err := newCFAccessClient(ctx, r.Client)
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "disabled") {
			return nil
		}
		return err
	}
	apps, err := ac.listApps(ctx)
	if err != nil {
		return err
	}
	hosts := append([]string{vs.Host}, vs.AdditionalHosts...)
	for _, host := range hosts {
		if host == "" {
			continue
		}
		if err := ac.deleteAccessAppByHost(ctx, apps, host); err != nil {
			return err
		}
	}
	return nil
}
