package portal

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

type stubCaller struct {
	denied string
}

func (s *stubCaller) CallTool(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, error) {
	if name == s.denied {
		return nil, context.Canceled // any error; denylist check fires first
	}
	return json.RawMessage(`{"ok":true}`), nil
}
func (s *stubCaller) ListTools(ctx context.Context) ([]string, error) {
	return []string{"list_stacks", "delete_stack"}, nil
}
func (s *stubCaller) ToolSchemas(ctx context.Context) (json.RawMessage, error) {
	return json.RawMessage(`[{"name":"list_stacks"}]`), nil
}
func (s *stubCaller) ToolCount() int    { return 2 }
func (s *stubCaller) AuthEnabled() bool { return false }

func TestMCPInfoAndDenylist(t *testing.T) {
	p := newFake(t)
	p.SetMCPCaller(&stubCaller{denied: "delete_stack"})
	h := p.Mux()

	// Info reflects the bridge.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://localhost/api/mcp", nil))
	var info MCPInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if !info.Enabled || len(info.Tools) != 2 {
		t.Fatalf("info: %+v", info)
	}

	// Allowed tool passes through.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "http://localhost/api/mcp/call",
		strings.NewReader(`{"name":"list_stacks","arguments":{}}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("allowed tool: %d %s", rec.Code, rec.Body.String())
	}

	// Destructive tool blocked at the portal layer.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "http://localhost/api/mcp/call",
		strings.NewReader(`{"name":"delete_stack","arguments":{"name":"x","namespace":"y","confirm":"y"}}`)))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "blocked") {
		t.Fatalf("delete_stack not blocked: %d %s", rec.Code, rec.Body.String())
	}

}
