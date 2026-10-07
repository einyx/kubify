package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type auth0Application struct {
	ID             string            `json:"client_id,omitempty"`
	Secret         string            `json:"client_secret,omitempty"`
	Name           string            `json:"name"`
	AppType        string            `json:"app_type"`
	Callbacks      []string          `json:"callbacks,omitempty"`
	LogoutURLs     []string          `json:"allowed_logout_urls,omitempty"`
	WebOrigins     []string          `json:"web_origins,omitempty"`
	AllowedOrigins []string          `json:"allowed_origins,omitempty"`
	Metadata       map[string]string `json:"client_metadata,omitempty"`
}

type auth0Client struct {
	http                                               *http.Client
	tokenURL, audience, apiURL, clientID, clientSecret string
	mu                                                 sync.Mutex
	token                                              string
	expires                                            time.Time
}

func (c *auth0Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expires) > time.Minute {
		return c.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.clientID}, "client_secret": {c.clientSecret}, "audience": {c.audience}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", auth0ResponseError(resp)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("Auth0 token response contained no access token")
	}
	c.token, c.expires = out.AccessToken, time.Now().Add(time.Duration(out.ExpiresIn)*time.Second)
	return c.token, nil
}

func (c *auth0Client) do(ctx context.Context, method, path string, body any, out any) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.apiURL, "/")+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return auth0ResponseError(resp)
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func auth0ResponseError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("Auth0 API %s: %s", resp.Status, strings.TrimSpace(string(b)))
}

func (c *auth0Client) create(ctx context.Context, app auth0Application) (auth0Application, error) {
	var out auth0Application
	err := c.do(ctx, http.MethodPost, "/clients", app, &out)
	return out, err
}
func (c *auth0Client) update(ctx context.Context, id string, app auth0Application) error {
	app.ID = ""
	app.Secret = ""
	return c.do(ctx, http.MethodPatch, "/clients/"+url.PathEscape(id), app, nil)
}
func (c *auth0Client) delete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/clients/"+url.PathEscape(id), nil, nil)
}
func (c *auth0Client) rotate(ctx context.Context, id string) (string, error) {
	var out struct {
		Secret string `json:"client_secret"`
	}
	err := c.do(ctx, http.MethodPost, "/clients/"+url.PathEscape(id)+"/rotate-secret", nil, &out)
	return out.Secret, err
}
