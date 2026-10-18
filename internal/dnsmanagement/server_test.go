package dnsmanagement

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeProvider struct {
	records []Record
	writes  int
	err     error
	last    Record
}

func (p *fakeProvider) Records(context.Context, string) ([]Record, error) { return p.records, p.err }
func (p *fakeProvider) Create(_ context.Context, _ string, r Record) (Record, error) {
	p.writes++
	p.last = r
	r.ID = "new"
	return r, p.err
}
func (p *fakeProvider) Update(_ context.Context, _, _ string, r Record) (Record, error) {
	p.writes++
	p.last = r
	return r, p.err
}
func (p *fakeProvider) Delete(context.Context, string, string) error { p.writes++; return p.err }

func fixture(t *testing.T, p *fakeProvider) http.Handler {
	t.Helper()
	h, err := NewServer(Config{Tenant: "tenant-a", Token: strings.Repeat("a", 32), Zones: map[string]string{"example.com": "zone1"}}, p)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func call(h http.Handler, method, path, body string, auth bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if auth {
		r.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestAuthorizationAndListing(t *testing.T) {
	p := &fakeProvider{records: []Record{{ID: "one", Type: "A", Name: "www.example.com", Content: "192.0.2.1", TTL: 300}}}
	h := fixture(t, p)
	for _, tt := range []struct {
		path   string
		auth   bool
		status int
	}{
		{"/health", false, 200}, {"/v1/zones", false, 401}, {"/v1/zones", true, 200},
		{"/v1/zones/other.test/records", true, 403}, {"/v1/zones/EXAMPLE.COM./records", true, 200},
		{"/v1/zones/example.com/records?limit=1001", true, 400},
		{"/v1/zones/example.com/records?offset=-1", true, 400},
	} {
		t.Run(tt.path, func(t *testing.T) {
			if w := call(h, "GET", tt.path, "", tt.auth); w.Code != tt.status {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
		})
	}
	if p.writes != 0 {
		t.Fatal("listing mutated DNS")
	}
}
func TestMutationsAndOwnership(t *testing.T) {
	p := &fakeProvider{records: []Record{{ID: "one", Type: "A", Name: "www.example.com", Content: "192.0.2.1", TTL: 1, Proxied: boolPtr(true)}}}
	h := fixture(t, p)
	body := `{"type":"A","name":"www.example.com","content":"192.0.2.2","ttl":1}`
	if w := call(h, "PUT", "/v1/zones/example.com/records/one", body, true); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if p.writes != 1 {
		t.Fatal("update missing")
	}
	if p.last.Proxied == nil || !*p.last.Proxied {
		t.Fatal("omitted proxy state was not preserved")
	}
	p.records[0].Comment = "managed-by:kubo stack=ns/app"
	for _, method := range []string{"PUT", "DELETE", "POST"} {
		path := "/v1/zones/example.com/records/one"
		if method == "POST" {
			path = "/v1/zones/example.com/records"
		}
		if w := call(h, method, path, body, true); w.Code != 403 {
			t.Fatalf("%s: %d %s", method, w.Code, w.Body)
		}
	}
	if p.writes != 1 {
		t.Fatal("controller-owned record mutated")
	}
	p.records[0].Comment = "human"
	if w := call(h, "DELETE", "/v1/zones/example.com/records/one", "", true); w.Code != 204 {
		t.Fatal(w.Code, w.Body)
	}
	if w := call(h, "POST", "/v1/zones/example.com/records", body, true); w.Code != 201 {
		t.Fatal(w.Code, w.Body)
	}
}

func TestRecordValidation(t *testing.T) {
	priority := uint16(10)
	for _, r := range []Record{
		{Type: "AAAA", Name: "v6.example.com", Content: "2001:db8::1", TTL: 300},
		{Type: "CNAME", Name: "alias.example.com", Content: "target.example.com.", TTL: 300},
		{Type: "NS", Name: "sub.example.com", Content: "ns.example.com", TTL: 300},
		{Type: "TXT", Name: "_acme-challenge.example.com", Content: "proof", TTL: 300},
		{Type: "MX", Name: "@", Content: "mail.example.com", TTL: 300, Priority: &priority},
		{Type: "CAA", Name: "example.com", Content: `0 issue "letsencrypt.org"`, TTL: 300},
		{Type: "SRV", Name: "_sip._tcp.example.com", Content: "10 20 5060 sip.example.com", TTL: 300},
	} {
		if err := validateRecord(&r, "example.com"); err != nil {
			t.Fatalf("%s: %v", r.Type, err)
		}
	}
	for _, r := range []Record{
		{Type: "TXT", Name: "bad<name.example.com", TTL: 300},
		{Type: "MX", Name: "example.com", Content: "mail.example.com", TTL: 300},
		{Type: "CNAME", Name: "alias.example.com", Content: "bad target", TTL: 300},
		{Type: "A", Name: "www.example.com", Content: "192.0.2.1", TTL: 2},
		{Type: "UNKNOWN", Name: "www.example.com", TTL: 300},
		{Type: "A", Name: "www.example.com", Content: "192.0.2.1", TTL: 300, Comment: "managed-by:kubo"},
	} {
		if err := validateRecord(&r, "example.com"); err == nil {
			t.Fatalf("accepted invalid record: %+v", r)
		}
	}
}

type blockingProvider struct {
	fakeProvider
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockingProvider) Records(ctx context.Context, _ string) ([]Record, error) {
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.release:
		return []Record{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func TestCancellationWhileWaitingForZone(t *testing.T) {
	p := &blockingProvider{entered: make(chan struct{}), release: make(chan struct{})}
	h, err := NewServer(Config{Tenant: "t", Token: strings.Repeat("a", 32), Zones: map[string]string{"example.com": "zone1"}}, p)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); call(h, "GET", "/v1/zones/example.com/records", "", true) }()
	<-p.entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("GET", "/v1/zones/example.com/records", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 32))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	close(p.release)
	<-done
	if w.Code != 504 {
		t.Fatal(w.Code, w.Body)
	}
}
func TestInputAndProviderFailures(t *testing.T) {
	p := &fakeProvider{}
	h := fixture(t, p)
	for _, body := range []string{
		`{"type":"A","name":"outside.test","content":"192.0.2.1","ttl":300}`,
		`{"type":"A","name":"www.example.com","content":"bad","ttl":300}`,
		`{"type":"A","name":"www.example.com","content":"192.0.2.1","ttl":300,"proxied":true}`,
		`{"type":"TXT","name":"www.example.com","content":"x","ttl":1,"proxied":true}`,
		`{"tenant":"other"}`, `{} {}`, `null`, strings.Repeat("x", 65537),
	} {
		if w := call(h, "POST", "/v1/zones/example.com/records", body, true); w.Code != 400 {
			t.Fatal(w.Code, w.Body)
		}
	}
	if w := call(h, "PUT", "/v1/zones/example.com/records/missing", `{"type":"A","name":"www.example.com","content":"192.0.2.1","ttl":300}`, true); w.Code != 404 {
		t.Fatal(w.Code, w.Body)
	}
	p.err = errors.New("secret provider failure")
	w := call(h, "GET", "/v1/zones/example.com/records", "", true)
	if w.Code != 502 || strings.Contains(w.Body.String(), "secret") {
		t.Fatal(w.Code, w.Body)
	}
	if p.writes != 0 {
		t.Fatal("invalid request mutated provider")
	}
}
func TestConfigFailsClosed(t *testing.T) {
	for _, cfg := range []Config{{}, {Tenant: "a", Token: "short", Zones: map[string]string{"example.com": "one"}},
		{Tenant: "a", Token: strings.Repeat("a", 32), Zones: map[string]string{}},
		{Tenant: "a", Token: strings.Repeat("a", 32), Zones: map[string]string{"bad domain": "one"}}} {
		if _, err := NewServer(cfg, &fakeProvider{}); err == nil {
			t.Fatal("accepted unsafe config")
		}
	}
}
