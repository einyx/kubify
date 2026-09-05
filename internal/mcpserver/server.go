// Package mcpserver implements an MCP (Model Context Protocol) server that
// exposes Stack Platform resources as tools callable by AI agents and Claude Code.
//
// Transport: SSE (GET /sse opens the stream, POST /message sends JSON-RPC 2.0).
// The server is a controller-runtime Runnable and lifecycle-managed by the operator manager.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	"github.com/einyx/kubo/internal/portal"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Server is an MCP server exposing Stack Platform tools over SSE.
// Implements sigs.k8s.io/controller-runtime/pkg/manager.Runnable.
type Server struct {
	Client client.Client
	Addr   string
	// Token, when set, requires Authorization: Bearer <token> on the SSE
	// and message endpoints. Empty disables auth (cluster-internal use only).
	Token string
	// Portal, when set, enables template tools (list_templates,
	// create_stack_from_template) backed by the portal's template registry.
	Portal *portal.Portal

	mu       sync.Mutex
	sessions map[string]*session
}

// New creates a Server. addr is the listen address, e.g. ":9090".
func New(c client.Client, addr string) *Server {
	return &Server{
		Client:   c,
		Addr:     addr,
		sessions: make(map[string]*session),
	}
}

// Start implements manager.Runnable.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", s.handleSSE)
	mux.HandleFunc("/message", s.handleMessage)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

	srv := &http.Server{
		Addr:         s.Addr,
		Handler:      wrapTokenAuth(mux, s.Token),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // SSE streams are long-lived
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("mcp server: %w", err)
	}
	return nil
}

// --- SSE transport -------------------------------------------------------

type session struct {
	id  string
	ch  chan []byte
	ctx context.Context
}

func (s *Server) newSession(ctx context.Context) *session {
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	sess := &session{id: id, ch: make(chan []byte, 32), ctx: ctx}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
	return sess
}

func (s *Server) removeSession(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// handleSSE opens an SSE connection, sends the endpoint event, and streams
// JSON-RPC responses back to the client.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	sess := s.newSession(r.Context())
	defer s.removeSession(sess.id)

	// Send the MCP endpoint event so the client knows where to POST messages.
	fmt.Fprintf(w, "event: endpoint\ndata: /message?sessionId=%s\n\n", sess.id)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-sess.ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

// handleMessage receives a JSON-RPC 2.0 POST from the client.
func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	sessionID := r.URL.Query().Get("sessionId")
	s.mu.Lock()
	sess, ok := s.sessions[sessionID]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	var req jsonRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, nil, -32700, "parse error")
		return
	}

	resp := s.dispatch(r.Context(), &req)
	raw, _ := json.Marshal(resp)
	sess.ch <- raw

	w.WriteHeader(http.StatusAccepted)
}

// --- JSON-RPC 2.0 types --------------------------------------------------

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, id interface{}, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg},
	})
}

func okResp(id, result interface{}) *jsonRPCResponse {
	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errResp(id interface{}, code int, msg string) *jsonRPCResponse {
	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// --- MCP dispatch --------------------------------------------------------

func (s *Server) dispatch(ctx context.Context, req *jsonRPCRequest) *jsonRPCResponse {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "tools/list":
		return okResp(req.ID, map[string]interface{}{"tools": toolList()})
	case "tools/call":
		return s.handleToolCall(ctx, req)
	default:
		return errResp(req.ID, -32601, "method not found: "+req.Method)
	}
}

func (s *Server) handleInitialize(req *jsonRPCRequest) *jsonRPCResponse {
	return okResp(req.ID, map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"serverInfo": map[string]string{
			"name":    "kubify-operator",
			"version": "0.1.0",
		},
		"capabilities": map[string]interface{}{
			"tools": map[string]bool{"listChanged": false},
		},
	})
}

// --- Tool registry -------------------------------------------------------

type toolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
}

func toolList() []toolDef {
	str := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	props := func(kv ...interface{}) map[string]interface{} {
		m := map[string]interface{}{}
		for i := 0; i < len(kv)-1; i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	schema := func(required []string, properties map[string]interface{}) map[string]interface{} {
		return map[string]interface{}{
			"type":       "object",
			"properties": properties,
			"required":   required,
		}
	}

	return []toolDef{
		{
			Name:        "list_stacks",
			Description: "List all Stack CRs. Optionally filter by namespace or phase.",
			InputSchema: schema(nil, props(
				"namespace", str("Kubernetes namespace to filter by. Empty = all namespaces."),
				"phase", str("Filter by phase: Pending, Progressing, Ready, Failed. Empty = all."),
			)),
		},
		{
			Name:        "get_stack",
			Description: "Get full status of a Stack: phase, components, last reconcile, conditions.",
			InputSchema: schema([]string{"name", "namespace"}, props(
				"name", str("Stack CR name."),
				"namespace", str("Namespace the Stack lives in."),
			)),
		},
		{
			Name:        "get_component",
			Description: "Get a single StackRelease: phase, Helm revision, last deployed timestamp.",
			InputSchema: schema([]string{"name", "namespace"}, props(
				"name", str("StackRelease name (format: <stack>-<component>)."),
				"namespace", str("Namespace the StackRelease lives in."),
			)),
		},
		{
			Name:        "pause_stack",
			Description: "Set spec.paused=true on a Stack. Stops reconciliation without deleting components.",
			InputSchema: schema([]string{"name", "namespace"}, props(
				"name", str("Stack CR name."),
				"namespace", str("Namespace the Stack lives in."),
			)),
		},
		{
			Name:        "resume_stack",
			Description: "Set spec.paused=false on a Stack. Resumes reconciliation.",
			InputSchema: schema([]string{"name", "namespace"}, props(
				"name", str("Stack CR name."),
				"namespace", str("Namespace the Stack lives in."),
			)),
		},
		{
			Name:        "trigger_reconcile",
			Description: "Annotate a Stack to force an immediate reconcile loop.",
			InputSchema: schema([]string{"name", "namespace"}, props(
				"name", str("Stack CR name."),
				"namespace", str("Namespace the Stack lives in."),
			)),
		},
		{
			Name:        "update_stack_values",
			Description: "Patch spec.componentValues on a Stack. Values are deep-merged with existing.",
			InputSchema: schema([]string{"name", "namespace", "component", "values"}, props(
				"name", str("Stack CR name."),
				"namespace", str("Namespace the Stack lives in."),
				"component", str("Component name within the Stack."),
				"values", map[string]interface{}{"type": "object", "description": "Helm values to set for the component (JSON object)."},
			)),
		},
		{
			Name:        "get_stack_yaml",
			Description: "Get the full Stack manifest as YAML.",
			InputSchema: schema([]string{"name", "namespace"}, props(
				"name", str("Stack CR name."),
				"namespace", str("Namespace the Stack lives in."),
			)),
		},
		{
			Name:        "get_events",
			Description: "Recent Warning events in a Stack namespace (newest first).",
			InputSchema: schema(nil, props(
				"namespace", str("Namespace to read events from."),
				"limit", map[string]interface{}{"type": "number", "description": "Max events (default 15, max 50)."},
			)),
		},
		{
			Name:        "diagnose_stack",
			Description: "One-call triage: stack status + non-ready components + recent warning events.",
			InputSchema: schema([]string{"name", "namespace"}, props(
				"name", str("Stack CR name."),
				"namespace", str("Namespace the Stack lives in."),
			)),
		},
		{
			Name:        "get_flags",
			Description: "Read the MX_FF_* feature flags of a component from the Stack spec.",
			InputSchema: schema([]string{"name", "namespace", "component"}, props(
				"name", str("Stack CR name."),
				"namespace", str("Namespace the Stack lives in."),
				"component", str("Component name (e.g. frontend, backend)."),
			)),
		},
		{
			Name:        "set_flags",
			Description: "Set MX_FF_* feature flags on a component. The operator rolls the workload to apply them — expect a brief restart.",
			InputSchema: schema([]string{"name", "namespace", "component", "flags"}, props(
				"name", str("Stack CR name."),
				"namespace", str("Namespace the Stack lives in."),
				"component", str("Component name."),
				"flags", map[string]interface{}{"type": "object", "description": "MX_FF_* keys with string values (e.g. {\"MX_FF_CONNECTORS_ENABLED\":\"true\"})."},
			)),
		},
		{
			Name:        "list_backups",
			Description: "List StackBackup resources (optionally by namespace).",
			InputSchema: schema(nil, props(
				"namespace", str("Filter by namespace. Empty = all."),
			)),
		},
		{
			Name:        "create_backup",
			Description: "Create a StackBackup: copies Postgres + storage-engine S3 state from a source to a target Stack namespace.",
			InputSchema: schema([]string{"name", "namespace", "source", "target"}, props(
				"name", str("StackBackup name."),
				"namespace", str("Namespace the StackBackup lives in (usually the source namespace)."),
				"source", str("Source Stack namespace."),
				"target", str("Target Stack namespace."),
				"include", map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Subset to copy: database, s3. Empty = both."},
			)),
		},
		{
			Name:        "get_stack_backup",
			Description: "Status of one StackBackup: phase, job, timestamps, message.",
			InputSchema: schema([]string{"name", "namespace"}, props(
				"name", str("StackBackup name."),
				"namespace", str("Namespace it lives in."),
			)),
		},
		{
			Name:        "get_vault_health",
			Description: "Whether a namespace's Vault is installed and its root token is tracked. Read-only.",
			InputSchema: schema([]string{"namespace"}, props(
				"namespace", str("Tenant namespace."),
			)),
		},
		{
			Name:        "list_templates",
			Description: "List stack templates available for create_stack_from_template.",
			InputSchema: schema(nil, nil),
		},
		{
			Name:        "create_stack_from_template",
			Description: "Create a new Stack (namespace + CR) from a template. Use dry_run=true to preview the YAML first.",
			InputSchema: schema([]string{"template", "tenant"}, props(
				"template", str("Template id (see list_templates)."),
				"tenant", str("Tenant slug — becomes namespace foundation-<tenant>."),
				"mode", str("Deployment mode: Direct (default) or Flux."),
				"dry_run", map[string]interface{}{"type": "boolean", "description": "Preview without creating."},
			)),
		},
		{
			Name:        "delete_stack",
			Description: "Delete a Stack CR. The finalizer uninstalls all component releases. GUARDED: confirm must equal the namespace. Prefer pause_stack for temporary stops.",
			InputSchema: schema([]string{"name", "namespace", "confirm"}, props(
				"name", str("Stack CR name."),
				"namespace", str("Namespace the Stack lives in."),
				"confirm", str("Must equal the namespace — typed confirmation against accidental deletes."),
			)),
		},
	}
}

// --- Tool call handler ---------------------------------------------------

func (s *Server) handleToolCall(ctx context.Context, req *jsonRPCRequest) *jsonRPCResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return errResp(req.ID, -32602, "invalid params")
	}

	var args map[string]interface{}
	if len(params.Arguments) > 0 {
		_ = json.Unmarshal(params.Arguments, &args)
	}
	if args == nil {
		args = map[string]interface{}{}
	}

	str := func(k string) string {
		v, _ := args[k].(string)
		return v
	}

	var (
		result interface{}
		err    error
	)

	switch params.Name {
	case "list_stacks":
		result, err = s.toolListStacks(ctx, str("namespace"), str("phase"))
	case "get_stack":
		result, err = s.toolGetStack(ctx, str("name"), str("namespace"))
	case "get_component":
		result, err = s.toolGetComponent(ctx, str("name"), str("namespace"))
	case "pause_stack":
		result, err = s.toolSetPaused(ctx, str("name"), str("namespace"), true)
	case "resume_stack":
		result, err = s.toolSetPaused(ctx, str("name"), str("namespace"), false)
	case "trigger_reconcile":
		result, err = s.toolTriggerReconcile(ctx, str("name"), str("namespace"))
	case "update_stack_values":
		var vals json.RawMessage
		var raw map[string]json.RawMessage
		if e := json.Unmarshal(params.Arguments, &raw); e == nil {
			vals = raw["values"]
		}
		result, err = s.toolUpdateValues(ctx, str("name"), str("namespace"), str("component"), vals)
	case "get_stack_yaml":
		result, err = s.toolGetStackYAML(ctx, str("name"), str("namespace"))
	case "get_events":
		limit := 0
		if n, ok := args["limit"].(float64); ok {
			limit = int(n)
		}
		result, err = s.toolGetEvents(ctx, str("namespace"), limit)
	case "diagnose_stack":
		result, err = s.toolDiagnoseStack(ctx, str("name"), str("namespace"))
	case "get_flags":
		result, err = s.toolGetFlags(ctx, str("name"), str("namespace"), str("component"))
	case "set_flags":
		flags := map[string]string{}
		if raw, ok := args["flags"].(map[string]interface{}); ok {
			for k, v := range raw {
				switch tv := v.(type) {
				case string:
					flags[k] = tv
				case bool:
					if tv {
						flags[k] = "true"
					} else {
						flags[k] = "false"
					}
				}
			}
		}
		result, err = s.toolSetFlags(ctx, str("name"), str("namespace"), str("component"), flags)
	case "list_backups":
		result, err = s.toolListBackups(ctx, str("namespace"))
	case "create_backup":
		var include []string
		if raw, ok := args["include"].([]interface{}); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok {
					include = append(include, s)
				}
			}
		}
		result, err = s.toolCreateBackup(ctx, str("name"), str("namespace"), str("source"), str("target"), include)
	case "get_stack_backup":
		result, err = s.toolGetBackup(ctx, str("name"), str("namespace"))
	case "get_vault_health":
		result, err = s.toolGetVaultHealth(ctx, str("namespace"))
	case "list_templates":
		result, err = s.toolListTemplates(ctx)
	case "create_stack_from_template":
		dry := false
		if v, ok := args["dry_run"].(bool); ok {
			dry = v
		}
		result, err = s.toolCreateFromTemplate(ctx, str("template"), str("tenant"), str("mode"), dry)
	case "delete_stack":
		result, err = s.toolDeleteStack(ctx, str("name"), str("namespace"), str("confirm"))
	default:
		return errResp(req.ID, -32601, "unknown tool: "+params.Name)
	}

	if err != nil {
		return errResp(req.ID, -32000, err.Error())
	}

	return okResp(req.ID, map[string]interface{}{
		"content": []map[string]interface{}{
			{"type": "text", "text": mustJSON(result)},
		},
	})
}

// --- Tool implementations ------------------------------------------------

func (s *Server) toolListStacks(ctx context.Context, namespace, phase string) (interface{}, error) {
	var list platformv1alpha1.StackList
	opts := []client.ListOption{}
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := s.Client.List(ctx, &list, opts...); err != nil {
		return nil, fmt.Errorf("list stacks: %w", err)
	}

	type row struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Phase     string `json:"phase"`
		Paused    bool   `json:"paused"`
		Bundle    string `json:"bundle,omitempty"`
		Mode      string `json:"mode"`
		Age       string `json:"age"`
	}

	var rows []row
	for _, st := range list.Items {
		p := string(st.Status.Phase)
		if phase != "" && p != phase {
			continue
		}
		bundle := ""
		if st.Spec.Bundle != nil {
			bundle = st.Spec.Bundle.URL
		}
		rows = append(rows, row{
			Name:      st.Name,
			Namespace: st.Namespace,
			Phase:     p,
			Paused:    st.Spec.Paused,
			Bundle:    bundle,
			Mode:      string(st.Spec.Mode),
			Age:       time.Since(st.CreationTimestamp.Time).Round(time.Second).String(),
		})
	}
	if rows == nil {
		rows = []row{}
	}
	return rows, nil
}

func (s *Server) toolGetStack(ctx context.Context, name, namespace string) (interface{}, error) {
	var st platformv1alpha1.Stack
	if err := s.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &st); err != nil {
		return nil, fmt.Errorf("get stack %s/%s: %w", namespace, name, err)
	}

	// Fetch StackReleases for this stack.
	var releases platformv1alpha1.StackReleaseList
	_ = s.Client.List(ctx, &releases,
		client.InNamespace(namespace),
		client.MatchingLabels{"stack": name},
	)

	type compRow struct {
		Component string `json:"component"`
		Phase     string `json:"phase"`
		Revision  int    `json:"revision"`
		Message   string `json:"message,omitempty"`
	}
	var comps []compRow
	for _, r := range releases.Items {
		comps = append(comps, compRow{
			Component: r.Spec.Component,
			Phase:     string(r.Status.Phase),
			Revision:  r.Status.Revision,
			Message:   r.Status.Message,
		})
	}

	bundle := ""
	if st.Spec.Bundle != nil {
		bundle = st.Spec.Bundle.URL
	}

	return map[string]interface{}{
		"name":       st.Name,
		"namespace":  st.Namespace,
		"phase":      string(st.Status.Phase),
		"paused":     st.Spec.Paused,
		"mode":       string(st.Spec.Mode),
		"bundle":     bundle,
		"stackRef":   st.Spec.StackRef,
		"components": comps,
		"age":        time.Since(st.CreationTimestamp.Time).Round(time.Second).String(),
	}, nil
}

func (s *Server) toolGetComponent(ctx context.Context, name, namespace string) (interface{}, error) {
	var sr platformv1alpha1.StackRelease
	if err := s.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &sr); err != nil {
		return nil, fmt.Errorf("get stackrelease %s/%s: %w", namespace, name, err)
	}

	lastDeployed := ""
	if sr.Status.LastDeployedAt != nil {
		lastDeployed = sr.Status.LastDeployedAt.UTC().Format(time.RFC3339)
	}

	return map[string]interface{}{
		"name":         sr.Name,
		"namespace":    sr.Namespace,
		"stack":        sr.Spec.StackRef,
		"component":    sr.Spec.Component,
		"phase":        string(sr.Status.Phase),
		"revision":     sr.Status.Revision,
		"message":      sr.Status.Message,
		"lastDeployed": lastDeployed,
	}, nil
}

func (s *Server) toolSetPaused(ctx context.Context, name, namespace string, paused bool) (interface{}, error) {
	var st platformv1alpha1.Stack
	if err := s.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &st); err != nil {
		return nil, fmt.Errorf("get stack: %w", err)
	}
	patch := client.MergeFrom(st.DeepCopy())
	st.Spec.Paused = paused
	if err := s.Client.Patch(ctx, &st, patch); err != nil {
		return nil, fmt.Errorf("patch stack: %w", err)
	}
	action := "paused"
	if !paused {
		action = "resumed"
	}
	return map[string]string{"status": action, "stack": name, "namespace": namespace}, nil
}

func (s *Server) toolTriggerReconcile(ctx context.Context, name, namespace string) (interface{}, error) {
	var st platformv1alpha1.Stack
	if err := s.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &st); err != nil {
		return nil, fmt.Errorf("get stack: %w", err)
	}
	patch := client.MergeFrom(st.DeepCopy())
	if st.Annotations == nil {
		st.Annotations = map[string]string{}
	}
	st.Annotations["kubify.io/reconcile-at"] = time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.Client.Patch(ctx, &st, patch); err != nil {
		return nil, fmt.Errorf("patch stack: %w", err)
	}
	return map[string]string{"status": "reconcile triggered", "stack": name, "namespace": namespace}, nil
}

func (s *Server) toolUpdateValues(ctx context.Context, name, namespace, component string, values json.RawMessage) (interface{}, error) {
	if component == "" {
		return nil, fmt.Errorf("component is required")
	}
	var st platformv1alpha1.Stack
	if err := s.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &st); err != nil {
		return nil, fmt.Errorf("get stack: %w", err)
	}
	patch := client.MergeFrom(st.DeepCopy())
	if st.Spec.ComponentValues == nil {
		st.Spec.ComponentValues = map[string]apiextensionsv1.JSON{}
	}
	st.Spec.ComponentValues[component] = apiextensionsv1.JSON{Raw: values}
	if err := s.Client.Patch(ctx, &st, patch); err != nil {
		return nil, fmt.Errorf("patch stack: %w", err)
	}
	return map[string]string{"status": "updated", "stack": name, "namespace": namespace, "component": component}, nil
}

// --- helpers -------------------------------------------------------------

func mustJSON(v interface{}) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// --- Portal bridge ----------------------------------------------------------
// The portal calls these in-process (it cannot import this package without
// an import cycle, so main.go wires an adapter through the Portal's caller
// interface).

// CallTool runs one tool by name with the given arguments and returns the
// decoded result payload (what result.content[0].text holds as JSON).
func (s *Server) CallTool(ctx context.Context, name string, arguments json.RawMessage) (json.RawMessage, error) {
	req := &jsonRPCRequest{ID: 1, Method: "tools/call", Params: rawJSON(map[string]interface{}{
		"name":      name,
		"arguments": arguments,
	})}
	resp := s.dispatch(ctx, req)
	if resp.Error != nil {
		return nil, fmt.Errorf("%s", resp.Error.Message)
	}
	content, _ := json.Marshal(resp.Result)
	var envelope struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(content, &envelope); err != nil {
		return nil, err
	}
	for _, c := range envelope.Content {
		if c.Type == "text" {
			return json.RawMessage(c.Text), nil
		}
	}
	return json.RawMessage("{}"), nil
}

// ToolCount returns the number of registered tools.
func (s *Server) ToolCount() int { return len(toolList()) }

// AuthEnabled reports whether the SSE transport requires a bearer token.
func (s *Server) AuthEnabled() bool { return s.Token != "" }

func rawJSON(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return json.RawMessage(b)
}

func rawJSONMap(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return json.RawMessage(b)
}

// ListTools returns the sorted tool names.
func (s *Server) ListTools(ctx context.Context) ([]string, error) {
	names := []string{}
	for _, t := range toolList() {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names, nil
}

// ToolSchemas returns the full tool definitions (name, description,
// inputSchema) for clients that want to render forms.
func (s *Server) ToolSchemas(ctx context.Context) (json.RawMessage, error) {
	b, err := json.Marshal(toolList())
	if err != nil {
		return nil, err
	}
	return b, nil
}
