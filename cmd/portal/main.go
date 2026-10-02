// Command portal serves the kubo operator portal: a light web UI to inspect
// Stack resources and create new stacks from the foundation demo template.
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/einyx/kubo/internal/portal"
)

func main() {
	addr := flag.String("addr", ":9090", "listen address")
	flag.Parse()

	p, err := portal.NewInCluster()
	if err != nil {
		log.Fatalf("portal: %v", err)
	}
	log.Printf("kubo portal listening on %s", *addr)
	if err := http.ListenAndServe(*addr, p.Mux()); err != nil {
		log.Fatalf("portal: %v", err)
	}
}
