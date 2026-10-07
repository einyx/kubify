package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnsureAccessAppSeparatesSSOAndServiceAuth(t *testing.T) {
	decisions := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"success":true,"result":[{"id":"sso-id","name":"kubo-sso","decision":"allow","include":[{"service_token":{"token_id":"token-id"}}]},{"id":"service-id","name":"kubo-service-auth","decision":"non_identity","include":[]}]}`))
			return
		}
		var body struct {
			Name, Decision string
			Include        []map[string]any
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		decisions[body.Name] = body.Decision
		_, _ = w.Write([]byte(`{"success":true,"result":{}}`))
	}))
	defer srv.Close()
	oldBase := cfAPIBase
	cfAPIBase = srv.URL
	defer func() { cfAPIBase = oldBase }()
	ac := &cfAccessClient{cfDNSClient: &cfDNSClient{token: "api-token", hc: srv.Client()}, accountID: "account", idpID: "idp", emailDomain: "meshx.io", serviceTokenID: "token-id"}
	apps := []cfAccessApp{{ID: "app-id", Name: "kubo:integration.meshx.foundation", Domain: "integration.meshx.foundation", AllowedIDPs: []string{"idp"}, AutoRedirectToIdentity: true}}
	if _, err := ac.ensureAccessApp(context.Background(), apps, "integration.meshx.foundation"); err != nil {
		t.Fatal(err)
	}
	if decisions["kubo-sso"] != "allow" {
		t.Fatalf("SSO decision = %q", decisions["kubo-sso"])
	}
	if decisions["kubo-service-auth"] != "non_identity" {
		t.Fatalf("service decision = %q", decisions["kubo-service-auth"])
	}
}
