package marketplace

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

const (
	defaultAPI = "https://marketplaceapi.microsoft.com/api/saas"
	apiVersion = "2018-08-31"
	// Microsoft Marketplace SaaS API application ID. The v2 token endpoint
	// requires this application ID as the scope rather than the API URL.
	marketplaceResource = "20e940b3-4c77-4b0b-9a53-9e16a1b010a7/"
)

type Config struct {
	TenantID, ClientID, ClientSecret string
	APIBase                          string
	HTTPClient                       *http.Client
}

type Client struct {
	cfg     Config
	mu      sync.Mutex
	token   string
	expires time.Time
}

type Party struct {
	EmailID  string `json:"emailId"`
	ObjectID string `json:"objectId"`
	TenantID string `json:"tenantId"`
}

type Subscription struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	OfferID     string `json:"offerId"`
	PlanID      string `json:"planId"`
	Quantity    int32  `json:"quantity"`
	Status      string `json:"saasSubscriptionStatus"`
	Beneficiary Party  `json:"beneficiary"`
	Purchaser   Party  `json:"purchaser"`
}

type ResolveResponse struct {
	ID               string       `json:"id"`
	SubscriptionName string       `json:"subscriptionName"`
	OfferID          string       `json:"offerId"`
	PlanID           string       `json:"planId"`
	Quantity         int32        `json:"quantity"`
	Subscription     Subscription `json:"subscription"`
}

// Operation is Microsoft's authoritative representation of a lifecycle
// operation. Webhook payloads are not trusted until this record is retrieved.
type Operation struct {
	ID             string `json:"id"`
	ActivityID     string `json:"activityId"`
	SubscriptionID string `json:"subscriptionId"`
	OfferID        string `json:"offerId"`
	PlanID         string `json:"planId"`
	Quantity       int32  `json:"quantity"`
	Action         string `json:"action"`
	Status         string `json:"status"`
}

func New(cfg Config) (*Client, error) {
	if cfg.TenantID == "" || cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, fmt.Errorf("marketplace credentials require tenant ID, client ID, and client secret")
	}
	if cfg.APIBase == "" {
		cfg.APIBase = defaultAPI
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{cfg: cfg}, nil
}

func (c *Client) Resolve(ctx context.Context, purchaseToken string) (ResolveResponse, error) {
	var out ResolveResponse
	err := c.do(ctx, http.MethodPost, "/subscriptions/resolve", nil, map[string]string{"x-ms-marketplace-token": purchaseToken}, &out)
	return out, err
}

func (c *Client) Activate(ctx context.Context, subscriptionID, planID string, quantity int32) error {
	body := map[string]interface{}{"planId": planID}
	if quantity > 0 {
		body["quantity"] = quantity
	}
	return c.do(ctx, http.MethodPost, "/subscriptions/"+url.PathEscape(subscriptionID)+"/activate", body, nil, nil)
}

func (c *Client) Get(ctx context.Context, subscriptionID string) (Subscription, error) {
	var out Subscription
	err := c.do(ctx, http.MethodGet, "/subscriptions/"+url.PathEscape(subscriptionID), nil, nil, &out)
	return out, err
}

// GetOperation validates a Marketplace webhook notification against
// Microsoft's fulfillment service before any local state is changed.
func (c *Client) GetOperation(ctx context.Context, subscriptionID, operationID string) (Operation, error) {
	var out Operation
	path := "/subscriptions/" + url.PathEscape(subscriptionID) + "/operations/" + url.PathEscape(operationID)
	err := c.do(ctx, http.MethodGet, path, nil, nil, &out)
	return out, err
}

func (c *Client) do(ctx context.Context, method, path string, body interface{}, headers map[string]string, out interface{}) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	var r io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		r = bytes.NewReader(b)
	}
	u := strings.TrimRight(c.cfg.APIBase, "/") + path + "?api-version=" + apiVersion
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-ms-requestid", fmt.Sprintf("kubo-%d", time.Now().UnixNano()))
	req.Header.Set("x-ms-correlationid", fmt.Sprintf("kubo-%d", time.Now().UnixNano()))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("marketplace %s %s: %s: %s", method, path, resp.Status, string(data))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode marketplace response: %w", err)
		}
	}
	return nil
}

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expires) > time.Minute {
		return c.token, nil
	}
	form := url.Values{"client_id": {c.cfg.ClientID}, "client_secret": {c.cfg.ClientSecret}, "grant_type": {"client_credentials"}, "scope": {marketplaceResource + ".default"}}
	u := "https://login.microsoftonline.com/" + url.PathEscape(c.cfg.TenantID) + "/oauth2/v2.0/token"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken      string `json:"access_token"`
		ExpiresIn        int    `json:"expires_in"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || tr.AccessToken == "" {
		return "", fmt.Errorf("marketplace token: %s: %s", resp.Status, tr.ErrorDescription)
	}
	c.token = tr.AccessToken
	c.expires = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return c.token, nil
}
