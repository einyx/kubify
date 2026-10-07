package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuth0ClientUsesManagementAudienceAndBearerToken(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if got := r.Form.Get("audience"); got != "https://tenant.auth0.com/api/v2/" {
				t.Fatalf("audience = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "management-token", "expires_in": 3600})
		case "/api/v2/clients":
			if got := r.Header.Get("Authorization"); got != "Bearer management-token" {
				t.Fatalf("authorization = %q", got)
			}
			if r.Method != http.MethodPost {
				t.Fatalf("method = %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(auth0Application{ID: "client-1", Secret: "generated-secret"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := &auth0Client{http: server.Client(), tokenURL: server.URL + "/oauth/token", audience: "https://tenant.auth0.com/api/v2/", apiURL: server.URL + "/api/v2", clientID: "manager", clientSecret: "secret"}
	created, err := c.create(context.Background(), auth0Application{Name: "integration", AppType: "regular_web"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "client-1" || created.Secret != "generated-secret" {
		t.Fatalf("unexpected response: %#v", created)
	}
}
