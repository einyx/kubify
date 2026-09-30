package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/einyx/kubo/internal/agentfw"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	adminAddr := flag.String("admin", ":8081", "admin endpoint address (kill switch)")
	policy := flag.String("policy", "/etc/agentfw/policy.yaml", "policy file path")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("agentfw listening on %s (admin: %s)", *addr, *adminAddr)
	if err := agentfw.Serve(ctx, *addr, *adminAddr, *policy); err != nil {
		log.Printf("agentfw: %v", err)
	}
}
