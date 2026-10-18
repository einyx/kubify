// Package dnsmanagement implements Kubo's optional in-process DNS management API.
package dnsmanagement

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/libdns/libdns"
)

type Record struct {
	ID       string  `json:"id,omitempty"`
	Type     string  `json:"type"`
	Name     string  `json:"name"`
	Content  string  `json:"content"`
	TTL      int     `json:"ttl"`
	Priority *uint16 `json:"priority,omitempty"`
	Proxied  *bool   `json:"proxied,omitempty"`
	Comment  string  `json:"comment,omitempty"`
	ReadOnly bool    `json:"read_only,omitempty"`
}

// RR exposes libdns's portable record representation without dropping API metadata.
func (r Record) RR() libdns.RR {
	data := r.Content
	if r.Type == "MX" && r.Priority != nil {
		data = fmt.Sprintf("%d %s", *r.Priority, r.Content)
	}
	return libdns.RR{Type: r.Type, Name: r.Name, TTL: time.Duration(r.TTL) * time.Second, Data: data}
}

type Provider interface {
	Records(context.Context, string) ([]Record, error)
	Create(context.Context, string, Record) (Record, error)
	Update(context.Context, string, string, Record) (Record, error)
	Delete(context.Context, string, string) error
}

// Config binds a private service instance to one tenant and explicit provider zones.
// Keys are ASCII zone names, values are provider zone IDs. No caller can change them.
type Config struct {
	Tenant string
	Token  string
	Zones  map[string]string
}

var domainPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var recordLabelPattern = regexp.MustCompile(`^[a-z0-9_](?:[a-z0-9_-]{0,61}[a-z0-9_])?$`)
var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func domain(name string) (string, error) {
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if name == "" || len(name) > 253 {
		return "", errors.New("invalid ASCII domain")
	}
	for _, label := range strings.Split(name, ".") {
		if !domainPattern.MatchString(label) {
			return "", errors.New("invalid ASCII domain")
		}
	}
	return name, nil
}

func boolPtr(value bool) *bool { return &value }
func protected(r Record) bool  { return strings.HasPrefix(r.Comment, "managed-by:kubo") }

type server struct {
	cfg      Config
	provider Provider
	locks    map[string]chan struct{}
}

func NewServer(cfg Config, p Provider) (http.Handler, error) {
	if strings.TrimSpace(cfg.Tenant) == "" || len(cfg.Token) < 32 || len(cfg.Zones) == 0 || p == nil {
		return nil, errors.New("tenant, service token (32+ bytes), zones and provider are required")
	}
	copyCfg := Config{Tenant: cfg.Tenant, Token: cfg.Token, Zones: map[string]string{}}
	s := &server{cfg: copyCfg, provider: p, locks: map[string]chan struct{}{}}
	for name, id := range cfg.Zones {
		normalized, err := domain(name)
		if err != nil || !identifierPattern.MatchString(id) {
			return nil, errors.New("invalid zone configuration")
		}
		if _, exists := s.cfg.Zones[normalized]; exists {
			return nil, errors.New("duplicate normalized zone")
		}
		s.cfg.Zones[normalized] = id
		s.locks[normalized] = make(chan struct{}, 1)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { reply(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /v1/zones", s.zones)
	mux.HandleFunc("GET /v1/zones/{zone}/records", s.records)
	mux.HandleFunc("POST /v1/zones/{zone}/records", s.records)
	mux.HandleFunc("PUT /v1/zones/{zone}/records/{id}", s.records)
	mux.HandleFunc("DELETE /v1/zones/{zone}/records/{id}", s.records)
	expected := sha256.Sum256([]byte(cfg.Token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path != "/health" || r.Method != "GET" {
			supplied, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			got := sha256.Sum256([]byte(supplied))
			if !ok || subtle.ConstantTimeCompare(expected[:], got[:]) != 1 {
				fail(w, 401, "unauthorized")
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		mux.ServeHTTP(w, r.WithContext(ctx))
	}), nil
}

func (s *server) zones(w http.ResponseWriter, r *http.Request) {
	names := make([]string, 0, len(s.cfg.Zones))
	for name := range s.cfg.Zones {
		names = append(names, name)
	}
	sort.Strings(names)
	reply(w, 200, map[string]any{"tenant": s.cfg.Tenant, "zones": names})
}

func (s *server) records(w http.ResponseWriter, r *http.Request) {
	zone, err := domain(r.PathValue("zone"))
	if err != nil {
		fail(w, 400, "invalid zone")
		return
	}
	zoneID, ok := s.cfg.Zones[zone]
	if !ok {
		fail(w, 403, "zone not authorized")
		return
	}
	id := r.PathValue("id")
	if id != "" && !identifierPattern.MatchString(id) {
		fail(w, 400, "invalid record id")
		return
	}
	limit, offset := 100, 0
	if r.Method == "GET" {
		if raw := r.URL.Query().Get("limit"); raw != "" {
			limit, err = strconv.Atoi(raw)
			if err != nil {
				limit = 0
			}
		}
		if raw := r.URL.Query().Get("offset"); raw != "" {
			offset, err = strconv.Atoi(raw)
			if err != nil {
				offset = -1
			}
		}
		if limit < 1 || limit > 1000 || offset < 0 {
			fail(w, 400, "invalid pagination")
			return
		}
	}
	var desired Record
	if r.Method == "POST" || r.Method == "PUT" {
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&desired); err != nil {
			fail(w, 400, "invalid JSON object")
			return
		}
		if decoder.Decode(new(any)) != io.EOF {
			fail(w, 400, "expected one JSON object")
			return
		}
		if err := validateRecord(&desired, zone); err != nil {
			fail(w, 400, err.Error())
			return
		}
	}
	// Serialize read-check-write against other requests in this instance.
	lock := s.locks[zone]
	select {
	case lock <- struct{}{}:
		defer func() { <-lock }()
	case <-r.Context().Done():
		fail(w, http.StatusGatewayTimeout, "request cancelled while waiting for zone")
		return
	}
	records, err := s.provider.Records(r.Context(), zoneID)
	if err != nil {
		fail(w, 502, "DNS provider request failed")
		return
	}
	if r.Method == "GET" {
		for i := range records {
			records[i].ReadOnly = protected(records[i])
		}
		if offset > len(records) {
			offset = len(records)
		}
		end := len(records)
		if limit < end-offset {
			end = offset + limit
		}
		page := append([]Record{}, records[offset:end]...)
		reply(w, 200, map[string]any{"records": page, "total": len(records)})
		return
	}
	var current *Record
	for i := range records {
		if records[i].ID == id && id != "" {
			current = &records[i]
		}
		if protected(records[i]) && (records[i].ID == id || strings.EqualFold(strings.TrimSuffix(records[i].Name, "."), desired.Name)) {
			fail(w, 403, "controller-managed record; change its Kubo Stack")
			return
		}
	}
	if r.Method != "POST" && current == nil {
		fail(w, 404, "record not found")
		return
	}
	if r.Method == "DELETE" {
		if err := s.provider.Delete(r.Context(), zoneID, id); err != nil {
			fail(w, 502, "DNS provider request failed")
			return
		}
		w.WriteHeader(204)
		return
	}
	status := 201
	if current != nil {
		if desired.Type != current.Type {
			fail(w, 400, "record type cannot be changed in place")
			return
		}
		if desired.Proxied == nil {
			desired.Proxied = current.Proxied
		}
		desired.Comment = current.Comment
		if desired.Proxied != nil && *desired.Proxied && desired.TTL != 1 {
			fail(w, 400, "proxied records require ttl=1")
			return
		}
		desired, err = s.provider.Update(r.Context(), zoneID, id, desired)
		status = 200
	} else {
		desired, err = s.provider.Create(r.Context(), zoneID, desired)
	}
	if err != nil {
		fail(w, 502, "DNS provider request failed")
		return
	}
	reply(w, status, desired)
}

func validateRecord(r *Record, zone string) error {
	if r.ID != "" || r.ReadOnly || r.Comment != "" {
		return errors.New("id, read_only and comment are server-controlled")
	}
	r.Type = strings.ToUpper(r.Type)
	name := strings.ToLower(strings.TrimSuffix(r.Name, "."))
	if name == "@" {
		name = zone
	}
	if name != zone && !strings.HasSuffix(name, "."+zone) {
		return errors.New("record name must be a fully qualified name within the zone")
	}
	check := strings.TrimPrefix(name, "*.")
	// Underscores are valid in service/TXT record labels, not in zone names.
	for _, part := range strings.Split(check, ".") {
		if !recordLabelPattern.MatchString(part) {
			return errors.New("invalid record name")
		}
	}
	if len(name) > 253 {
		return errors.New("record name too long")
	}
	r.Name = name
	if r.TTL != 1 && (r.TTL < 30 || r.TTL > 86400) {
		return errors.New("ttl must be 1 or 30..86400")
	}
	if r.Proxied != nil && *r.Proxied && (r.TTL != 1 || (r.Type != "A" && r.Type != "AAAA" && r.Type != "CNAME")) {
		return errors.New("proxying requires A/AAAA/CNAME and ttl=1")
	}
	switch r.Type {
	case "A", "AAAA":
		addr, err := netip.ParseAddr(r.Content)
		if err != nil || (r.Type == "A" && !addr.Is4()) || (r.Type == "AAAA" && !addr.Is6()) {
			return errors.New("invalid IP address")
		}
	case "CNAME", "NS":
		if _, err := domain(r.Content); err != nil {
			return errors.New("invalid target domain")
		}
	case "MX":
		if r.Priority == nil {
			return errors.New("MX priority is required")
		}
		if _, err := domain(r.Content); err != nil {
			return errors.New("invalid MX target")
		}
	case "TXT":
		if len(r.Content) > 2048 {
			return errors.New("TXT content too long")
		}
	case "CAA", "SRV":
		if _, err := r.RR().Parse(); err != nil {
			return errors.New("invalid structured record content")
		}
	default:
		return errors.New("supported types: A AAAA CNAME NS MX TXT CAA SRV")
	}
	return nil
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, message string) {
	reply(w, status, map[string]string{"error": message})
}
