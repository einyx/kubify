package dnsmanagement

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCloudflarePaginationAndMutations(t *testing.T) {
	writes := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer provider-secret" {
			t.Error("missing provider auth")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			page := r.URL.Query().Get("page")
			if page == "" {
				page = "1"
			}
			fmt.Fprintf(w, `{"success":true,"result":[{"id":"record%s","type":"A","name":"www.example.com","content":"192.0.2.1","ttl":1,"proxied":true,"comment":"human"}],"result_info":{"total_pages":2}}`, page)
			return
		}
		writes++
		if r.Method == "DELETE" {
			fmt.Fprint(w, `{"success":true,"result":{"id":"record1"}}`)
			return
		}
		var input Record
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if input.Proxied == nil || *input.Proxied {
			t.Error("explicit proxy=false lost")
		}
		input.ID = "record1"
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": input})
	}))
	defer upstream.Close()
	p, err := NewCloudflare("provider-secret", upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	p.baseURL = upstream.URL
	records, err := p.Records(context.Background(), "zone1")
	if err != nil || len(records) != 2 {
		t.Fatal(records, err)
	}
	if !*records[0].Proxied || records[0].Comment != "human" {
		t.Fatal("metadata lost")
	}
	desired := records[0]
	desired.Proxied = boolPtr(false)
	if _, err = p.Create(context.Background(), "zone1", desired); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Update(context.Background(), "zone1", "record1", desired); err != nil {
		t.Fatal(err)
	}
	if err = p.Delete(context.Background(), "zone1", "record1"); err != nil {
		t.Fatal(err)
	}
	if writes != 3 {
		t.Fatal(writes)
	}
}
func TestCloudflareErrors(t *testing.T) {
	for _, body := range []string{`{"success":false,"result":[]}`, `not-json`, `{"success":true,"result":[],"result_info":{}}`} {
		t.Run(body, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer upstream.Close()
			p, _ := NewCloudflare("test", upstream.Client())
			p.baseURL = upstream.URL
			if _, err := p.Records(context.Background(), "zone1"); err == nil {
				t.Fatal("accepted malformed/failed provider snapshot")
			}
		})
	}
	if _, err := NewCloudflare("", nil); err == nil {
		t.Fatal("empty provider token")
	}
}

func TestCloudflareEmptyZone(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":true,"result":[],"result_info":{"total_pages":0}}`)
	}))
	defer upstream.Close()
	p, err := NewCloudflare("test", upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	p.baseURL = upstream.URL
	records, err := p.Records(context.Background(), "zone1")
	if err != nil || len(records) != 0 {
		t.Fatalf("empty zone: records=%v error=%v", records, err)
	}
}
