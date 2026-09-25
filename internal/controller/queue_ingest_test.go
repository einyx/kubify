package controller

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestDecodeQueuePayload(t *testing.T) {
	p, ok := decodeQueuePayload(`{"email":"Ada@Example.io","company":"Acme"}`)
	if !ok || p.email != "ada@example.io" || p.company != "Acme" {
		t.Fatalf("plain json decode = %+v ok=%v", p, ok)
	}
	enc := base64.StdEncoding.EncodeToString([]byte(`{"email":"x@y.io"}`))
	if p, ok := decodeQueuePayload(enc); !ok || p.email != "x@y.io" {
		t.Fatalf("base64 decode = %+v ok=%v", p, ok)
	}
	if _, ok := decodeQueuePayload("not json"); ok {
		t.Fatal("garbage accepted")
	}
	if _, ok := decodeQueuePayload(`{"email":"no-at-sign"}`); ok {
		t.Fatal("email without @ accepted")
	}
}

func TestQueueIngestDrain(t *testing.T) {
	var created, deleted int64
	var payloads []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			// The queue API answers in XML (camelCase elements), regardless
			// of the Accept header.
			fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<QueueMessagesList>
  <QueueMessage>
    <MessageId>m1</MessageId><PopReceipt>pr1</PopReceipt><DequeueCount>1</DequeueCount>
    <MessageText>%s</MessageText>
  </QueueMessage>
  <QueueMessage>
    <MessageId>m2</MessageId><PopReceipt>pr2</PopReceipt><DequeueCount>1</DequeueCount>
    <MessageText>garbage</MessageText>
  </QueueMessage>
</QueueMessagesList>`, base64.StdEncoding.EncodeToString([]byte(`{"email":"ada@acme.io","company":"Acme"}`)))
		case r.Method == http.MethodDelete:
			atomic.AddInt64(&deleted, 1)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	q := &QueueIngester{
		QueueURL:   srv.URL,
		HTTPClient: srv.Client(),
		CreateDemoRequest: func(ctx context.Context, email, company string) error {
			atomic.AddInt64(&created, 1)
			payloads = append(payloads, email+"/"+company)
			return nil
		},
	}
	q.drain(context.Background())
	if int(atomic.LoadInt64(&created)) != 1 {
		t.Fatalf("created = %d, want 1", created)
	}
	if int(atomic.LoadInt64(&deleted)) != 2 { // valid + poison both deleted
		t.Fatalf("deleted = %d, want 2", deleted)
	}
	if len(payloads) != 1 || payloads[0] != "ada@acme.io/Acme" {
		t.Fatalf("payloads = %v", payloads)
	}
}
