package portal

import (
	"net/http"

	"github.com/einyx/kubo/internal/dnsmanagement"
)

// SetDNSManagement enables the in-process DNS API. Configure before serving.
// The DNS handler always requires its own bearer token, including in RemoteMux;
// allow-remote does not grant DNS access. Never send the service token to a UI.
func (p *Portal) SetDNSManagement(cfg dnsmanagement.Config, provider dnsmanagement.Provider) error {
	handler, err := dnsmanagement.NewServer(cfg, provider)
	if err != nil {
		return err
	}
	p.dnsManagement = http.StripPrefix("/api/dns", handler)
	return nil
}

func (p *Portal) handleDNSManagement(w http.ResponseWriter, r *http.Request) {
	if p.dnsManagement == nil {
		http.NotFound(w, r)
		return
	}
	p.dnsManagement.ServeHTTP(w, r)
}
