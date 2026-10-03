package portal

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/einyx/kubo/internal/vaultkv"
)

// VaultEntryView is a Vault KV entry as returned by the API. Data values are
// redacted unless the request explicitly asked for a reveal.
type VaultEntryView struct {
	Namespace string            `json:"namespace"`
	Path      string            `json:"path"`
	Keys      []string          `json:"keys,omitempty"`
	Data      map[string]string `json:"data,omitempty"`
	Version   int               `json:"version,omitempty"`
	CreatedAt string            `json:"createdAt,omitempty"`
	Destroyed bool              `json:"destroyed,omitempty"`
}

// VaultHealth is the health snapshot of a namespace's Vault.
type VaultHealth struct {
	Installed bool   `json:"installed"`
	Sealed    bool   `json:"sealed"`
	Addr      string `json:"addr,omitempty"`
	Error     string `json:"error,omitempty"`
}

// VaultWriteRequest is the payload for writing a Vault entry.
type VaultWriteRequest struct {
	Path string            `json:"path"`
	Data map[string]string `json:"data"`
}

func redact(data map[string]string) map[string]string {
	out := map[string]string{}
	for k := range data {
		out[k] = "••••••••"
	}
	return out
}

// vaultClientFor returns an authenticated client for the namespace's Vault.
// The root token stays in server memory; responses never contain it.
func (p *Portal) vaultClientFor(ctx context.Context, ns string) (*vaultkv.Client, error) {
	if !validTenant(ns) {
		return nil, fmt.Errorf("invalid namespace %q", ns)
	}
	vc, err := vaultkv.NewForNamespace(ctx, p.client, ns)
	if err != nil {
		return nil, err
	}
	if p.vaultAddrOverride != nil {
		return vaultkv.NewWithAddr(p.vaultAddrOverride(ns), vc.Token()), nil
	}
	return vc, nil
}

// VaultHealth reports sealed/installed state for a namespace's Vault.
func (p *Portal) VaultHealth(ctx context.Context, ns string) (*VaultHealth, error) {
	if !validTenant(ns) {
		return nil, fmt.Errorf("invalid namespace %q", ns)
	}
	h := &VaultHealth{}
	vc, err := p.vaultClientFor(ctx, ns)
	if err != nil {
		h.Error = err.Error()
		return h, nil
	}
	h.Installed = true
	h.Addr = ""
	sealed, err := vc.Sealed(ctx)
	if err != nil {
		h.Error = err.Error()
		return h, nil
	}
	h.Sealed = sealed
	return h, nil
}

// VaultList lists entries and folders under a path.
func (p *Portal) VaultList(ctx context.Context, ns, path string) (entries, folders []string, err error) {
	vc, err := p.vaultClientFor(ctx, ns)
	if err != nil {
		return nil, nil, err
	}
	if sealed, serr := vc.Sealed(ctx); serr == nil && sealed {
		return nil, nil, fmt.Errorf("vault is sealed; unseal it first")
	}
	keys, err := vc.List(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	entries, folders = vaultkv.SplitList(keys)
	sort.Strings(entries)
	sort.Strings(folders)
	return entries, folders, nil
}

// VaultRead returns one entry; values are redacted unless reveal is true.
func (p *Portal) VaultRead(ctx context.Context, ns, path string, reveal bool) (*VaultEntryView, error) {
	vc, err := p.vaultClientFor(ctx, ns)
	if err != nil {
		return nil, err
	}
	e, err := vc.Read(ctx, path)
	if err != nil {
		return nil, err
	}
	view := &VaultEntryView{
		Namespace: ns,
		Path:      e.Path,
		Version:   e.Version,
		CreatedAt: e.CreatedAt,
		Destroyed: e.Destroyed,
		Keys:      sortedKeys(e.Data),
	}
	if reveal {
		view.Data = e.Data
	} else {
		view.Data = redact(e.Data)
	}
	return view, nil
}

// VaultWrite creates a new version of an entry.
func (p *Portal) VaultWrite(ctx context.Context, ns string, req VaultWriteRequest) error {
	if req.Path == "" {
		return fmt.Errorf("path is required")
	}
	if strings.Contains(req.Path, "..") {
		return fmt.Errorf("path must not contain '..'")
	}
	vc, err := p.vaultClientFor(ctx, ns)
	if err != nil {
		return err
	}
	if sealed, serr := vc.Sealed(ctx); serr == nil && sealed {
		return fmt.Errorf("vault is sealed; unseal it first")
	}
	return vc.Write(ctx, strings.Trim(req.Path, "/"), req.Data)
}

// VaultDelete removes an entry. Soft-delete by default; permanent destroys
// all versions and requires an explicit flag.
func (p *Portal) VaultDelete(ctx context.Context, ns, path string, permanent bool) error {
	if path == "" {
		return fmt.Errorf("path is required")
	}
	vc, err := p.vaultClientFor(ctx, ns)
	if err != nil {
		return err
	}
	return vc.Delete(ctx, strings.Trim(path, "/"), permanent)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
