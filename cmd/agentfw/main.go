package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/einyx/kubo/internal/agentfw"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mitm-ca" {
		mitmCA(os.Args[2:])
		return
	}

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

func mitmCA(args []string) {
	fs := flag.NewFlagSet("mitm-ca", flag.ExitOnError)
	out := fs.String("out", "./ca", "output directory for ca.crt + ca.key")
	fs.Parse(args)
	cert, key, err := agentfw.GenerateCA(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\nwrote %s\n", cert, key)
	fmt.Println("mount ca.crt into agent containers as SSL_CERT_FILE, keep ca.key on the proxy only.")
}
