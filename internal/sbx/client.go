package sbx

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const DefaultBaseURL = "https://connect.docker.com/sandboxes"

type CreateRequest struct {
	DisplayName string            `json:"displayName,omitempty"`
	Agent       string            `json:"agent,omitempty"`
	ImageRef    string            `json:"imageRef,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	PolicyIDs   []string          `json:"policyIds,omitempty"`
	Resources   *Resources        `json:"resources,omitempty"`
}

type Resources struct {
	CPUs      *int32 `json:"cpus,omitempty"`
	MemoryMiB *int64 `json:"memoryMib,omitempty"`
}

type Sandbox struct {
	Name string      `json:"name"`
	Core SandboxCore `json:"core"`
}

type SandboxCore struct {
	Status   string   `json:"status"`
	Endpoint Endpoint `json:"endpoint"`
}

type Endpoint struct {
	URI string `json:"uri"`
}

type ExecRequest struct {
	Cmd        []string          `json:"cmd"`
	Env        map[string]string `json:"env,omitempty"`
	WorkingDir string            `json:"workingDir,omitempty"`
}

type ExecResult struct {
	ExitCode   int32  `json:"exitCode"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Incomplete bool   `json:"incomplete"`
}

type Service interface {
	Create(context.Context, CreateRequest, string) (Sandbox, error)
	Get(context.Context, string) (Sandbox, error)
	Exec(context.Context, Sandbox, ExecRequest) (ExecResult, error)
	Delete(context.Context, string) error
}

type Client struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

func New(token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{BaseURL: DefaultBaseURL, Token: token, HTTPClient: httpClient}
}

func (c *Client) Create(ctx context.Context, in CreateRequest, idempotencyKey string) (Sandbox, error) {
	var out Sandbox
	err := c.do(ctx, c.BaseURL+"/v1/sandboxes", http.MethodPost, c.Token, in, map[string]string{"Idempotency-Key": idempotencyKey}, &out)
	return out, err
}

func (c *Client) Get(ctx context.Context, name string) (Sandbox, error) {
	var out Sandbox
	err := c.do(ctx, c.BaseURL+"/v1/sandboxes/"+url.PathEscape(resourceID(name)), http.MethodGet, c.Token, nil, nil, &out)
	return out, err
}

func (c *Client) Delete(ctx context.Context, name string) error {
	return c.do(ctx, c.BaseURL+"/v1/sandboxes/"+url.PathEscape(resourceID(name)), http.MethodDelete, c.Token, nil, nil, nil)
}

func (c *Client) Exec(ctx context.Context, sandbox Sandbox, in ExecRequest) (ExecResult, error) {
	var credential struct {
		Token string `json:"token"`
	}
	credentialURL := c.BaseURL + "/v1/sandboxes/" + url.PathEscape(resourceID(sandbox.Name)) + "/endpoint-credentials"
	if err := c.do(ctx, credentialURL, http.MethodPost, c.Token, map[string]any{"permissions": []string{"sandboxesExec"}}, nil, &credential); err != nil {
		return ExecResult{}, fmt.Errorf("create endpoint credential: %w", err)
	}
	var out ExecResult
	endpoint := strings.TrimRight(sandbox.Core.Endpoint.URI, "/") + "/v1/processes/exec"
	err := c.do(ctx, endpoint, http.MethodPost, credential.Token, in, nil, &out)
	decodeOutput(&out.Stdout)
	decodeOutput(&out.Stderr)
	return out, err
}

func (c *Client) do(ctx context.Context, endpoint, method, token string, body any, headers map[string]string, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("docker sandboxes API %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func resourceID(name string) string {
	parts := strings.Split(strings.Trim(name, "/"), "/")
	return parts[len(parts)-1]
}

// The API schema marks stdout/stderr as byte strings. Accept both base64 and
// plain strings to remain compatible with experimental server versions.
func decodeOutput(value *string) {
	decoded, err := base64.StdEncoding.DecodeString(*value)
	if err == nil {
		*value = string(decoded)
	}
}
