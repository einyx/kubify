// Queue ingester: drains website demo requests from an Azure Storage Queue
// into DemoRequest CRs. This is the subscribe/pull transport for callers
// that cannot reach the operator network (the static website's Azure
// Function enqueues; cross-subscription, no inbound exposure of the
// cluster). The push transport is the token-authed POST /api/demorequests.
package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

// QueueIngester polls an Azure Storage Queue via its REST API using a SAS
// URL scoped to read/process/delete on the queue.
type QueueIngester struct {
	// CreateDemoRequest inserts one DemoRequest (validation mirrors the
	// portal's push endpoint so both transports admit identically).
	CreateDemoRequest func(ctx context.Context, email, company, target string) error
	// QueueURL is the queue's base URL including the SAS token.
	QueueURL string
	// PollInterval between drains; defaults to 15s.
	PollInterval time.Duration
	// MaxDequeueCount before a message is dropped as poison.
	MaxDequeueCount int64
	HTTPClient      *http.Client
}

// queueMessage covers both wire formats: the REST API answers in XML
// (PascalCase elements), tests may use JSON (camelCase).
type queueMessage struct {
	MessageID      string `xml:"MessageId" json:"messageId"`
	PopReceipt     string `xml:"PopReceipt" json:"popReceipt"`
	Text           string `xml:"MessageText" json:"messageText"`
	DequeueCount   int64  `xml:"DequeueCount" json:"dequeueCount"`
	VisibilityTO   int    `xml:"VisibilityTimeout" json:"visibilityTimeout"`
	NextVisibleAt  string `xml:"NextVisibleTime" json:"nextVisibleTime"`
	InsertedAt     string `xml:"InsertionTime" json:"insertionTime"`
	ExpiresAt      string `xml:"ExpirationTime" json:"expirationTime"`
}

// Start blocks until ctx is done, draining the queue every PollInterval.
func (q *QueueIngester) Start(ctx context.Context) error {
	interval := q.PollInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if q.MaxDequeueCount <= 0 {
		q.MaxDequeueCount = 5
	}
	log := log.FromContext(ctx)
	log.Info("demo request queue ingester started", "interval", interval.String())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			q.drain(ctx)
		}
	}
}

// drain pulls up to 32 messages, creating DemoRequests and deleting handled
// messages. Failures leave the message to retry after its visibility
// timeout; messages over MaxDequeueCount are dropped (the queue's own
// poison handling moves them to -poison as the backstop).
func (q *QueueIngester) drain(ctx context.Context) {
	log := log.FromContext(ctx)
	if q.MaxDequeueCount <= 0 {
		q.MaxDequeueCount = 5
	}
	hc := q.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	msgs, err := q.dequeue(ctx, hc)
	if err != nil {
		if ctx.Err() == nil {
			log.Error(err, "demo queue dequeue failed")
		}
		return
	}
	for _, m := range msgs {
		if m.DequeueCount > q.MaxDequeueCount {
			_ = q.deleteMessage(ctx, hc, m)
			log.Info("demo queue poison message dropped", "id", m.MessageID, "dequeues", m.DequeueCount)
			continue
		}
		payload, ok := decodeQueuePayload(m.Text)
		if !ok {
			_ = q.deleteMessage(ctx, hc, m)
			log.Info("demo queue invalid message dropped", "id", m.MessageID)
			continue
		}
		if err := q.CreateDemoRequest(ctx, payload.email, payload.company, payload.target); err != nil {
			log.Error(err, "demo queue create failed; message returns to queue", "id", m.MessageID)
			continue
		}
		if err := q.deleteMessage(ctx, hc, m); err != nil {
			log.Error(err, "demo queue delete failed", "id", m.MessageID)
		}
	}
}

// queueURL builds a request URL against the queue base, merging extra
// query params into the SAS token's query string (the base URL already
// contains ?sig=..., so naive concatenation corrupts it).
func (q *QueueIngester) queueURL(path string, extra map[string]string) (string, error) {
	u, err := url.Parse(q.QueueURL)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	qs := u.Query()
	for k, v := range extra {
		qs.Set(k, v)
	}
	u.RawQuery = qs.Encode()
	return u.String(), nil
}

func (q *QueueIngester) dequeue(ctx context.Context, hc *http.Client) ([]queueMessage, error) {
	u, err := q.queueURL("/messages", map[string]string{"numofmessages": "32", "visibilitytimeout": "120"})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("demo queue not found (404)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("demo queue dequeue: HTTP %d", resp.StatusCode)
	}
	// The queue API returns XML regardless of Accept; parse that.
	var out struct {
		Messages []queueMessage `xml:"QueueMessage"`
	}
	if err := xml.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("demo queue decode: %w", err)
	}
	return out.Messages, nil
}

func (q *QueueIngester) deleteMessage(ctx context.Context, hc *http.Client, m queueMessage) error {
	u, err := q.queueURL("/messages/"+url.PathEscape(m.MessageID), map[string]string{"popreceipt": m.PopReceipt})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("demo queue delete: HTTP %d", resp.StatusCode)
	}
	return nil
}

type queuePayload struct {
	email, company, target string
}

// decodeQueuePayload accepts base64-encoded or plain JSON.
func decodeQueuePayload(text string) (queuePayload, bool) {
	raw := text
	if decoded, err := base64.StdEncoding.DecodeString(text); err == nil {
		raw = string(decoded)
	}
	var p struct {
		Email   string `json:"email"`
		Company string `json:"company"`
		Target  string `json:"target"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil || !strings.Contains(p.Email, "@") {
		return queuePayload{}, false
	}
	return queuePayload{
		email:   strings.ToLower(strings.TrimSpace(p.Email)),
		company: strings.TrimSpace(p.Company),
		target:  strings.ToLower(strings.TrimSpace(p.Target)),
	}, true
}

// StartQueueIngesterFromEnv wires the ingester when DEMO_QUEUE_URL is set
// (env), or falls back to the kubo-system/demo-request-queue secret (url
// key). Returns nil silently when unconfigured — push-only deployments.
func StartQueueIngesterFromEnv(ctx context.Context, reader client.Reader, c client.Client) error {
	queueURL := envOrController("DEMO_QUEUE_URL", "")
	if queueURL == "" {
		var s corev1.Secret
		// reader must be the uncached APIReader: at startup the manager's
		// cache is not synced yet and a cached read silently misses the
		// secret, leaving the ingester unconfigured.
		if err := reader.Get(ctx, client.ObjectKey{Namespace: "kubo-system", Name: "demo-request-queue"}, &s); err != nil {
			return nil // not configured: push-only
		}
		queueURL = string(s.Data["url"])
	}
	if queueURL == "" {
		return nil
	}
	ing := &QueueIngester{
		QueueURL: strings.TrimRight(queueURL, "/"),
		CreateDemoRequest: func(ctx context.Context, email, company, target string) error {
			dr := platformv1alpha1.DemoRequest{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "demo-", Namespace: "kubo-system"},
				Spec:       platformv1alpha1.DemoRequestSpec{Email: email, Company: company, Target: target, Template: "full"},
			}
			return c.Create(ctx, &dr)
		},
	}
	go func() {
		if err := ing.Start(ctx); err != nil {
			log.FromContext(ctx).Error(err, "demo request queue ingester stopped")
		}
	}()
	log.FromContext(ctx).Info("demo request queue ingester configured")
	return nil
}

// envOrController returns the env var or def when unset/empty.
func envOrController(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
