package sbx

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientCreateUsesIdempotencyKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer control-token" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("Idempotency-Key"); got != "delivery-1" {
			t.Errorf("idempotency key = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"sandboxes/abc","core":{"status":"creating","endpoint":{}}}`))
	}))
	defer server.Close()
	c := New("control-token", server.Client())
	c.BaseURL = server.URL
	sandbox, err := c.Create(context.Background(), CreateRequest{Agent: "claude"}, "delivery-1")
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.Name != "sandboxes/abc" {
		t.Fatalf("name = %q", sandbox.Name)
	}
}

func TestClientExecUsesScopedCredential(t *testing.T) {
	var endpointToken string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/sandboxes/abc/endpoint-credentials":
			_, _ = w.Write([]byte(`{"token":"endpoint-token"}`))
		case "/v1/processes/exec":
			endpointToken = r.Header.Get("Authorization")
			stdout := base64.StdEncoding.EncodeToString([]byte("done\n"))
			_, _ = w.Write([]byte(`{"exitCode":0,"stdout":"` + stdout + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := New("control-token", server.Client())
	c.BaseURL = server.URL
	result, err := c.Exec(context.Background(), Sandbox{Name: "sandboxes/abc", Core: SandboxCore{Endpoint: Endpoint{URI: server.URL}}}, ExecRequest{Cmd: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if endpointToken != "Bearer endpoint-token" {
		t.Fatalf("endpoint authorization = %q", endpointToken)
	}
	if result.Stdout != "done\n" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
}
