package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newServer(objs ...runtime.Object) *Server {
	scheme := runtime.NewScheme()
	_ = platformv1alpha1.AddToScheme(scheme)
	b := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&platformv1alpha1.Stack{}, &platformv1alpha1.StackRelease{})
	if len(objs) > 0 {
		b = b.WithRuntimeObjects(objs...)
	}
	return New(b.Build(), ":0")
}

func call(t *testing.T, s *Server, method string, params interface{}) *jsonRPCResponse {
	t.Helper()
	raw, _ := json.Marshal(params)
	req := &jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: raw}
	resp := s.dispatch(context.Background(), req)
	if resp.Error != nil {
		t.Fatalf("unexpected rpc error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp
}

func toolCall(t *testing.T, s *Server, name string, args interface{}) string {
	t.Helper()
	argsRaw, _ := json.Marshal(args)
	resp := call(t, s, "tools/call", map[string]interface{}{
		"name":      name,
		"arguments": json.RawMessage(argsRaw),
	})
	// result.content[0].text
	var outer struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	b, _ := json.Marshal(resp.Result)
	_ = json.Unmarshal(b, &outer)
	return outer.Content[0].Text
}

// --- initialize ----------------------------------------------------------

func TestInitialize(t *testing.T) {
	s := newServer()
	resp := call(t, s, "initialize", map[string]interface{}{})
	b, _ := json.Marshal(resp.Result)
	var r map[string]interface{}
	_ = json.Unmarshal(b, &r)
	if r["protocolVersion"] != "2024-11-05" {
		t.Errorf("unexpected protocolVersion: %v", r["protocolVersion"])
	}
}

// --- tools/list ----------------------------------------------------------

func TestToolsList(t *testing.T) {
	s := newServer()
	resp := call(t, s, "tools/list", map[string]interface{}{})
	b, _ := json.Marshal(resp.Result)
	var r struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(b, &r)
	names := make(map[string]bool)
	for _, tool := range r.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"list_stacks", "get_stack", "pause_stack", "resume_stack", "trigger_reconcile", "update_stack_values"} {
		if !names[want] {
			t.Errorf("missing tool %q", want)
		}
	}
}

// --- list_stacks ---------------------------------------------------------

func TestListStacks_empty(t *testing.T) {
	s := newServer()
	text := toolCall(t, s, "list_stacks", map[string]string{})
	if !strings.Contains(text, "[]") {
		t.Errorf("expected empty array, got %q", text)
	}
}

func TestListStacks_withStack(t *testing.T) {
	st := &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "acme"},
		Spec:       platformv1alpha1.StackSpec{StackRef: "acme", Mode: platformv1alpha1.DeploymentModeDirect},
	}
	s := newServer(st)
	text := toolCall(t, s, "list_stacks", map[string]string{})
	if !strings.Contains(text, "acme") {
		t.Errorf("expected acme in response, got %q", text)
	}
}

// --- get_stack -----------------------------------------------------------

func TestGetStack(t *testing.T) {
	st := &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "acme"},
		Spec:       platformv1alpha1.StackSpec{StackRef: "acme"},
	}
	s := newServer(st)
	text := toolCall(t, s, "get_stack", map[string]string{"name": "acme", "namespace": "acme"})
	if !strings.Contains(text, `"stackRef": "acme"`) {
		t.Errorf("expected stackRef in response, got %q", text)
	}
}

func TestGetStack_notFound(t *testing.T) {
	s := newServer()
	argsRaw, _ := json.Marshal(map[string]string{"name": "missing", "namespace": "x"})
	resp := s.dispatch(context.Background(), &jsonRPCRequest{
		JSONRPC: "2.0", ID: 1, Method: "tools/call",
		Params: mustMarshal(map[string]interface{}{"name": "get_stack", "arguments": json.RawMessage(argsRaw)}),
	})
	if resp.Error == nil {
		t.Fatal("expected error for missing stack")
	}
}

// --- pause / resume ------------------------------------------------------

func TestPauseResume(t *testing.T) {
	st := &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "acme"},
		Spec:       platformv1alpha1.StackSpec{StackRef: "acme"},
	}
	s := newServer(st)

	text := toolCall(t, s, "pause_stack", map[string]string{"name": "acme", "namespace": "acme"})
	if !strings.Contains(text, "paused") {
		t.Errorf("expected paused status, got %q", text)
	}

	text = toolCall(t, s, "resume_stack", map[string]string{"name": "acme", "namespace": "acme"})
	if !strings.Contains(text, "resumed") {
		t.Errorf("expected resumed status, got %q", text)
	}
}

// --- trigger_reconcile ---------------------------------------------------

func TestTriggerReconcile(t *testing.T) {
	st := &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "acme"},
	}
	s := newServer(st)
	text := toolCall(t, s, "trigger_reconcile", map[string]string{"name": "acme", "namespace": "acme"})
	if !strings.Contains(text, "triggered") {
		t.Errorf("expected triggered in response, got %q", text)
	}
}

// --- update_stack_values -------------------------------------------------

func TestUpdateStackValues(t *testing.T) {
	st := &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "acme"},
	}
	s := newServer(st)
	text := toolCall(t, s, "update_stack_values", map[string]interface{}{
		"name":      "acme",
		"namespace": "acme",
		"component": "backend",
		"values":    map[string]interface{}{"replicas": 3},
	})
	if !strings.Contains(text, "updated") {
		t.Errorf("expected updated status, got %q", text)
	}
}

func TestUpdateStackValues_noComponent(t *testing.T) {
	st := &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Name: "acme", Namespace: "acme"},
	}
	s := newServer(st)
	argsRaw, _ := json.Marshal(map[string]interface{}{"name": "acme", "namespace": "acme", "values": map[string]interface{}{}})
	resp := s.dispatch(context.Background(), &jsonRPCRequest{
		JSONRPC: "2.0", ID: 1, Method: "tools/call",
		Params: mustMarshal(map[string]interface{}{"name": "update_stack_values", "arguments": json.RawMessage(argsRaw)}),
	})
	if resp.Error == nil {
		t.Fatal("expected error when component is empty")
	}
}

// --- get_component -------------------------------------------------------

func TestGetComponent(t *testing.T) {
	now := metav1.Now()
	sr := &platformv1alpha1.StackRelease{
		ObjectMeta: metav1.ObjectMeta{Name: "acme-backend", Namespace: "acme"},
		Spec:       platformv1alpha1.StackReleaseSpec{StackRef: "acme", Component: "backend"},
		Status: platformv1alpha1.StackReleaseStatus{
			Phase:          platformv1alpha1.ComponentPhaseReady,
			Revision:       3,
			LastDeployedAt: &now,
		},
	}
	s := newServer(sr)
	text := toolCall(t, s, "get_component", map[string]string{"name": "acme-backend", "namespace": "acme"})
	if !strings.Contains(text, "backend") {
		t.Errorf("expected component name in response, got %q", text)
	}
	if !strings.Contains(text, "Ready") {
		t.Errorf("expected Ready phase, got %q", text)
	}
}

// --- SSE HTTP transport --------------------------------------------------

func TestSSEEndpointEvent(t *testing.T) {
	s := newServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse", nil)

	// Run SSE handler in a goroutine; cancel after reading the first event.
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleSSE(rec, req)
	}()

	// Give the handler a moment to write the endpoint event.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, "event: endpoint") {
		t.Errorf("expected endpoint event, got %q", body)
	}
	if !strings.Contains(body, "/message?sessionId=") {
		t.Errorf("expected sessionId in endpoint event, got %q", body)
	}
}

func TestMessageEndpoint_unknownSession(t *testing.T) {
	s := newServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/message?sessionId=nope", bytes.NewBufferString(`{}`))
	s.handleMessage(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestMessageEndpoint_wrongMethod(t *testing.T) {
	s := newServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/message", nil)
	s.handleMessage(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
}

// --- unknown tool / method -----------------------------------------------

func TestUnknownTool(t *testing.T) {
	s := newServer()
	resp := s.dispatch(context.Background(), &jsonRPCRequest{
		JSONRPC: "2.0", ID: 1, Method: "tools/call",
		Params: mustMarshal(map[string]interface{}{"name": "delete_everything", "arguments": map[string]interface{}{}}),
	})
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Errorf("expected -32601 for unknown tool, got %+v", resp.Error)
	}
}

func TestUnknownMethod(t *testing.T) {
	s := newServer()
	resp := s.dispatch(context.Background(), &jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "bogus"})
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Errorf("expected -32601, got %+v", resp.Error)
	}
}

// --- helpers -------------------------------------------------------------

func mustMarshal(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// Keep compiler happy for unused type reference.
var _ = apiextensionsv1.JSON{}
