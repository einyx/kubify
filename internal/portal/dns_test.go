package portal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/einyx/kubo/internal/dnsmanagement"
)

type dnsFakeProvider struct{ calls int }

func (p *dnsFakeProvider) Records(context.Context, string) ([]dnsmanagement.Record, error) {
	p.calls++
	return []dnsmanagement.Record{}, nil
}
func (*dnsFakeProvider) Create(context.Context, string, dnsmanagement.Record) (dnsmanagement.Record, error) {
	panic("unexpected write")
}
func (*dnsFakeProvider) Update(context.Context, string, string, dnsmanagement.Record) (dnsmanagement.Record, error) {
	panic("unexpected write")
}
func (*dnsFakeProvider) Delete(context.Context, string, string) error { panic("unexpected write") }

func TestDNSManagementMount(t *testing.T) {
	p := New(nil)
	request := func(h http.Handler, path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://localhost"+path, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request(p.Mux(), "/api/dns/v1/zones", ""); w.Code != 404 {
		t.Fatal(w.Code, w.Body)
	}
	provider := &dnsFakeProvider{}
	if err := p.SetDNSManagement(dnsmanagement.Config{}, provider); err == nil {
		t.Fatal("invalid configuration accepted")
	}
	token := strings.Repeat("s", 32)
	if err := p.SetDNSManagement(dnsmanagement.Config{Tenant: "tenant-a", Token: token, Zones: map[string]string{"example.com": "zone1"}}, provider); err != nil {
		t.Fatal(err)
	}
	for _, h := range []http.Handler{p.Mux(), p.RemoteMux()} {
		if w := request(h, "/api/dns/v1/zones", ""); w.Code != 401 {
			t.Fatal(w.Code, w.Body)
		}
		if w := request(h, "/api/dns/v1/zones", token); w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
		if w := request(h, "/api/dns/v1/zones/other.test/records", token); w.Code != 403 {
			t.Fatal(w.Code, w.Body)
		}
	}
	if provider.calls != 0 {
		t.Fatal("unauthorized zone reached provider")
	}
}
