// Command portal serves the kubo operator portal: a light web UI to inspect
// Stack resources and create new stacks from the product demo template.
package main

import (
	"flag"
	"log"
	"net"
	"net/http"

	"github.com/einyx/kubo/internal/portal"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9090", "listen address (loopback by default)")
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
