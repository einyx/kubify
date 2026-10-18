package dnsmanagement

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/libdns/libdns"
)

// Cloudflare is a small MIT adapter. libdns supplies record parsing; this adapter
// retains Cloudflare IDs, proxy state and comments, which generic records omit.
type Cloudflare struct {
	token   string
	client  *http.Client
	baseURL string
}

func NewCloudflare(token string, client *http.Client) (*Cloudflare, error) {
	if token == "" {
		return nil, errors.New("Cloudflare API token is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Cloudflare{token: token, client: &copyClient, baseURL: "https://api.cloudflare.com/client/v4"}, nil
}

type envelope struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Info    struct {
		Pages *int `json:"total_pages"`
	} `json:"result_info"`
}
type cfRecord struct {
	Record
	Data json.RawMessage `json:"data"`
}

func (p *Cloudflare) request(ctx context.Context, method, path string, body any) (envelope, error) {
	var response envelope
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return response, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, reader)
	if err != nil {
		return response, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return response, fmt.Errorf("Cloudflare transport: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return response, fmt.Errorf("Cloudflare HTTP %d", resp.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
	if err = decoder.Decode(&response); err != nil {
		return response, errors.New("invalid Cloudflare response")
	}
	if !response.Success || len(response.Result) == 0 || string(response.Result) == "null" {
		return response, errors.New("Cloudflare operation failed")
	}
	return response, nil
}

func (p *Cloudflare) Records(ctx context.Context, zoneID string) ([]Record, error) {
	records := []Record{}
	for page := 1; page <= 1000; page++ {
		path := "/zones/" + url.PathEscape(zoneID) + "/dns_records?per_page=100&page=" + strconv.Itoa(page)
		env, err := p.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		if env.Info.Pages == nil || *env.Info.Pages < 0 {
			return nil, errors.New("Cloudflare pagination metadata missing")
		}
		var batch []cfRecord
		if err = json.Unmarshal(env.Result, &batch); err != nil {
			return nil, err
		}
		if *env.Info.Pages == 0 && len(batch) != 0 {
			return nil, errors.New("inconsistent Cloudflare pagination")
		}
		for _, item := range batch {
			if item.ID == "" || item.Name == "" || item.Type == "" {
				return nil, errors.New("incomplete Cloudflare record")
			}
			record, err := item.record()
			if err != nil {
				return nil, err
			}
			records = append(records, record)
		}
		if page >= *env.Info.Pages {
			return records, nil
		}
	}
	return nil, errors.New("Cloudflare pagination limit exceeded")
}

func (r cfRecord) record() (Record, error) {
	if r.Type == "SRV" {
		var data struct {
			Priority, Weight, Port uint16
			Target                 string
		}
		if err := json.Unmarshal(r.Data, &data); err != nil || data.Target == "" {
			return Record{}, errors.New("invalid Cloudflare SRV data")
		}
		r.Content = fmt.Sprintf("%d %d %d %s", data.Priority, data.Weight, data.Port, data.Target)
	}
	return r.Record, nil
}

func (p *Cloudflare) Create(ctx context.Context, zoneID string, r Record) (Record, error) {
	return p.write(ctx, http.MethodPost, zoneID, "", r)
}
func (p *Cloudflare) Update(ctx context.Context, zoneID, id string, r Record) (Record, error) {
	return p.write(ctx, http.MethodPatch, zoneID, id, r)
}
func (p *Cloudflare) write(ctx context.Context, method, zoneID, id string, r Record) (Record, error) {
	body := map[string]any{"type": r.Type, "name": r.Name, "content": r.Content, "ttl": r.TTL}
	if r.Proxied != nil {
		body["proxied"] = *r.Proxied
	}
	if r.Priority != nil {
		body["priority"] = *r.Priority
	}
	if r.Comment != "" {
		body["comment"] = r.Comment
	}
	if r.Type == "CAA" || r.Type == "SRV" {
		parsed, err := r.RR().Parse()
		if err != nil {
			return Record{}, err
		}
		switch value := parsed.(type) {
		case libdns.CAA:
			body["data"] = map[string]any{"flags": value.Flags, "tag": value.Tag, "value": value.Value}
		case libdns.SRV:
			body["data"] = map[string]any{"priority": value.Priority, "weight": value.Weight, "port": value.Port, "target": value.Target}
		default:
			return Record{}, errors.New("unexpected structured DNS record")
		}
		delete(body, "content")
	}
	path := "/zones/" + url.PathEscape(zoneID) + "/dns_records"
	if id != "" {
		path += "/" + url.PathEscape(id)
	}
	env, err := p.request(ctx, method, path, body)
	if err != nil {
		return Record{}, err
	}
	var result cfRecord
	if err = json.Unmarshal(env.Result, &result); err != nil {
		return Record{}, err
	}
	if result.ID == "" {
		return Record{}, errors.New("missing Cloudflare record ID")
	}
	return result.record()
}
func (p *Cloudflare) Delete(ctx context.Context, zoneID, id string) error {
	_, err := p.request(ctx, http.MethodDelete, "/zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(id), nil)
	return err
}
