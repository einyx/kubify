// Command portal serves the kubo operator portal: a light web UI to inspect
// Stack resources and create new stacks from tenant templates.
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"strings"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	"github.com/einyx/kubo/internal/mcpserver"
	"github.com/einyx/kubo/internal/portal"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9090", "listen address (loopback by default)")
	templatesDir := flag.String("templates", portal.DefaultTemplatesDir, "local templates dir")
	agentfwURL := flag.String("agentfw", os.Getenv("AGENTFW_URL"),
		"agentfw admin endpoint for the /agentfw/ session archive (empty = disabled). "+
			"Accepts a direct URL (http://host:port) or svc:<namespace>/<service>[:<port>] — "+
			"the service-proxy form needs no port-forward")
	vaultAddrTpl := flag.String("vault-addr-template", os.Getenv("KUBO_VAULT_ADDR_TEMPLATE"),
		"Vault address template overriding the in-cluster default (http://vault.<ns>.svc.cluster.local:8200). "+
			"Use {ns} for the namespace, e.g. http://localhost:8200 for a single port-forwarded Vault")
	allowRemote := flag.Bool("allow-remote", false,
		"bind non-loopback addresses — the portal has NO authentication; "+
			"only do this behind an authenticating proxy or NetworkPolicy")
	flag.Parse()

	if !isLoopbackAddr(*addr) && !*allowRemote {
		log.Fatalf("portal: refusing to bind %q without -allow-remote: the portal is unauthenticated", *addr)
	}

	p, err := portal.NewInCluster()
	if err != nil {
		log.Fatalf("portal: %v", err)
	}

	// Bridge the portal's MCP section to the in-process MCP dispatch.
	sch := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = corev1.AddToScheme(sch)
	_ = platformv1alpha1.AddToScheme(sch)
	restCfg, cfgErr := config.GetConfig()
	if cfgErr == nil {
		if k8sClient, err := client.New(restCfg, client.Options{Scheme: sch}); err == nil {
			mcpSrv := mcpserver.New(k8sClient, "")
			mcpSrv.Portal = p // wire template tools (list_templates, create_from_template)
			p.SetMCPCaller(mcpSrv)
		}
	}
	p.SetTemplateDir(*templatesDir)
	if *vaultAddrTpl != "" {
		tpl := *vaultAddrTpl
		p.SetVaultAddrFunc(func(ns string) string {
			return strings.ReplaceAll(tpl, "{ns}", ns)
		})
		log.Printf("portal: vault addr template %s", tpl)
	}
	if err := p.SetAgentfwURL(*agentfwURL); err != nil {
		log.Fatalf("portal: %v", err)
	}
	if p.AgentfwEnabled() {
		log.Printf("portal: agentfw archive at /agentfw/ (upstream %s)", *agentfwURL)
	}
	log.Printf("kubo portal listening on %s", *addr)
	if err := http.ListenAndServe(*addr, p.Mux()); err != nil {
		log.Fatalf("portal: %v", err)
	}
}

func isLoopbackAddr(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host == "localhost"
	}
	return ip.IsLoopback()
}
