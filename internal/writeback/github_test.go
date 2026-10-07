package writeback

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateFeatureFlagPRReturnsExistingOpenPR(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/repos/meshxdata/flux":
			w.Write([]byte(`{"default_branch":"main"}`))
		case "/repos/meshxdata/flux/pulls":
			if got := r.URL.Query().Get("head"); got != "meshxdata:kubify/integration-product-id" {
				t.Fatalf("head query = %q", got)
			}
			w.Write([]byte(`[{"html_url":"https://github.com/meshxdata/flux/pull/1559","head":{"sha":"abc123"}}]`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()

	got, err := (&GitHub{APIBase: srv.URL, HTTPClient: srv.Client()}).CreateFeatureFlagPR(
		context.Background(), "https://github.com/meshxdata/flux.git", "stack.yaml", "integration", "product",
		map[string]string{"flag": "false"}, map[string]string{"flag": "true"}, "update", "kubify/integration-product-id",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://github.com/meshxdata/flux/pull/1559" || got.Commit != "abc123" {
		t.Fatalf("got %#v", got)
	}
	if requests != 2 {
		t.Fatalf("made %d requests, want 2", requests)
	}
}
