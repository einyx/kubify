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
	CreateDemoRequest func(ctx context.Context, email, company string) error
	// QueueURL is the queue's base URL including the SAS token.
	QueueURL string
	// PollInterval between drains; defaults to 15s.
	PollInterval time.Duration
	// MaxDequeueCount before a message is dropped as poison.
	MaxDequeueCount int64
	HTTPClient      *http.Client
}

type queueMessage struct {
	MessageID      string `json:"messageId"`
	PopReceipt     string `json:"popReceipt"`
	Text           string `json:"messageText"`
	DequeueCount   int64  `json:"dequeueCount"`
	VisibilityTO   int    `json:"visibilityTimeout"`
	NextVisibleAt  string `json:"nextVisibleTime"`
	InsertedAt     string `json:"insertionTime"`
	ExpiresAt      string `json:"expirationTime"`
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
		if err := q.CreateDemoRequest(ctx, payload.email, payload.company); err != nil {
			log.Error(err, "demo queue create failed; message returns to queue", "id", m.MessageID)
			continue
		}
		if err := q.deleteMessage(ctx, hc, m); err != nil {
			log.Error(err, "demo queue delete failed", "id", m.MessageID)
		}
	}
}

func (q *QueueIngester) dequeue(ctx context.Context, hc *http.Client) ([]queueMessage, error) {
	u := q.QueueURL + "/messages?numofmessages=32&visibilitytimeout=120"
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
	var out struct {
		Messages []queueMessage `json:"value"`
	}
	// The queue API returns XML by default; request JSON via Accept.
	if json.Unmarshal(body, &out) != nil || len(out.Messages) == 0 {
		return nil, nil
	}
	return out.Messages, nil
}

func (q *QueueIngester) deleteMessage(ctx context.Context, hc *http.Client, m queueMessage) error {
	u := fmt.Sprintf("%s/messages/%s?popreceipt=%s", q.QueueURL, url.PathEscape(m.MessageID), url.QueryEscape(m.PopReceipt))
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
	email, company string
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
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil || !strings.Contains(p.Email, "@") {
		return queuePayload{}, false
	}
	return queuePayload{email: strings.ToLower(strings.TrimSpace(p.Email)), company: strings.TrimSpace(p.Company)}, true
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
		CreateDemoRequest: func(ctx context.Context, email, company string) error {
			dr := platformv1alpha1.DemoRequest{
				ObjectMeta: metav1.ObjectMeta{GenerateName: "demo-", Namespace: "kubo-system"},
				Spec:       platformv1alpha1.DemoRequestSpec{Email: email, Company: company, Template: "full"},
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
