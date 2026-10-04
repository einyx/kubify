package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newFake(t *testing.T, objs ...client.Object) *Server {
	t.Helper()
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return New(fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build(), "")
}

func callTool(t *testing.T, s *Server, name string, args map[string]interface{}) (interface{}, error) {
	t.Helper()
	params, _ := json.Marshal(map[string]interface{}{"name": name, "arguments": args})
	req := &jsonRPCRequest{ID: 1, Method: "tools/call", Params: params}
	resp := s.dispatch(context.Background(), req)
	if resp.Error != nil {
		return nil, fmt.Errorf("%s", resp.Error.Message)
	}
	// Tool payloads ride in result.content[0].text as JSON — parse them back
	// so assertions see the same types a real MCP client would after decoding.
	content, _ := json.Marshal(resp.Result)
	var envelope struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(content, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Content) == 0 {
		return map[string]interface{}{}, nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(envelope.Content[0].Text), &out); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

func testStack() *platformv1alpha1.Stack {
	blob := []byte(`{"env":{"MX_FF_CONNECTORS_ENABLED":"true","MX_FF_VAULT":"false"}}`)
	return &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Name: "foundation", Namespace: "foundation-a"},
		Spec: platformv1alpha1.StackSpec{
			Mode: platformv1alpha1.DeploymentModeDirect,
			ComponentValues: map[string]apiextensionsv1.JSON{
				"frontend": {Raw: blob},
			},
		},
		Status: platformv1alpha1.StackStatus{Phase: "Progressing"},
	}
}

func TestToolGetSetFlags(t *testing.T) {
	s := newFake(t, testStack())

	res, err := callTool(t, s, "get_flags", map[string]interface{}{"name": "foundation", "namespace": "foundation-a", "component": "frontend"})
	if err != nil {
		t.Fatal(err)
	}
	flags := res.(map[string]interface{})["flags"].(map[string]interface{})
	if flags["MX_FF_CONNECTORS_ENABLED"] != "true" || flags["MX_FF_VAULT"] != "false" {
		t.Fatalf("unexpected flags: %v", flags)
	}

	_, err = callTool(t, s, "set_flags", map[string]interface{}{
		"name": "foundation", "namespace": "foundation-a", "component": "frontend",
		"flags": map[string]interface{}{"MX_FF_VAULT": "true", "MX_FF_NEW_ONE": "true"},
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err = callTool(t, s, "get_flags", map[string]interface{}{"name": "foundation", "namespace": "foundation-a", "component": "frontend"})
	if err != nil {
		t.Fatal(err)
	}
	flags = res.(map[string]interface{})["flags"].(map[string]interface{})
	if flags["MX_FF_VAULT"] != "true" || flags["MX_FF_NEW_ONE"] != "true" || flags["MX_FF_CONNECTORS_ENABLED"] != "true" {
		t.Fatalf("flags not persisted: %v", flags)
	}

	// Non-catalogue prefix rejected.
	if _, err = callTool(t, s, "set_flags", map[string]interface{}{
		"name": "foundation", "namespace": "foundation-a", "component": "frontend",
		"flags": map[string]interface{}{"NOT_A_FLAG": "true"},
	}); err == nil {
		t.Fatal("non-MX_FF_ flag accepted")
	}
}

func TestToolGetStackYAML(t *testing.T) {
	s := newFake(t, testStack())
	res, err := callTool(t, s, "get_stack_yaml", map[string]interface{}{"name": "foundation", "namespace": "foundation-a"})
	if err != nil {
		t.Fatal(err)
	}
	y := res.(map[string]interface{})["yaml"].(string)
	if !strings.Contains(y, "kind: Stack") || !strings.Contains(y, "foundation-a") {
		t.Fatalf("yaml: %s", y[:200])
	}
}

func TestToolGetEventsAndDiagnose(t *testing.T) {
	s := newFake(t, testStack())
	res, err := callTool(t, s, "diagnose_stack", map[string]interface{}{"name": "foundation", "namespace": "foundation-a"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]interface{})
	if _, ok := m["stack"]; !ok {
		t.Fatal("diagnose missing stack")
	}
	if _, ok := m["warningEvents"]; !ok {
		t.Fatal("diagnose missing warningEvents")
	}
}

func TestToolVaultHealth(t *testing.T) {
	s := newFake(t, testStack())
	res, err := callTool(t, s, "get_vault_health", map[string]interface{}{"namespace": "foundation-a"})
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]interface{})["installed"] != false {
		t.Fatalf("expected not-installed for missing keys secret")
	}
}

func TestToolDeleteStackConfirm(t *testing.T) {
	s := newFake(t, testStack())
	_, err := callTool(t, s, "delete_stack", map[string]interface{}{"name": "foundation", "namespace": "foundation-a", "confirm": "wrong"})
	if err == nil {
		t.Fatal("delete accepted with wrong confirm")
	}
	if _, err := s.toolGetStack(context.Background(), "foundation", "foundation-a"); err != nil {
		t.Fatal("stack deleted without valid confirm")
	}
}

func TestWrapTokenAuth(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	h := wrapTokenAuth(next, "tok")

	req := httptest.NewRequest("GET", "/sse", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || called {
		t.Fatal("request without token passed")
	}

	req = httptest.NewRequest("GET", "http://x/sse?token=tok", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called {
		t.Fatal("request with query token rejected")
	}

	called = false
	req = httptest.NewRequest("GET", "http://x/sse", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called {
		t.Fatal("request with bearer token rejected")
	}
}

func TestServeStdioRoundTrip(t *testing.T) {
	s := newFake(t, testStack())
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_stack","arguments":{"name":"foundation","namespace":"foundation-a"}}}`,
		`not-json`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope"}}`,
	}, "\n")
	var out bytes.Buffer
	if err := ServeStdio(context.Background(), s, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 responses, got %d", len(lines))
	}
	var init map[string]interface{}
	json.Unmarshal([]byte(lines[0]), &init)
	if init["result"] == nil {
		t.Fatal("initialize failed")
	}
	var tools map[string]interface{}
	json.Unmarshal([]byte(lines[1]), &tools)
	toolList := tools["result"].(map[string]interface{})["tools"].([]interface{})
	if len(toolList) < 16 {
		t.Fatalf("tool list too small: %d", len(toolList))
	}
	var bad map[string]interface{}
	json.Unmarshal([]byte(lines[3]), &bad)
	if bad["error"] == nil {
		t.Fatal("parse error not reported")
	}
}
