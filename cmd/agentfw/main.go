package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/einyx/kubo/internal/agentfw"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mitm-ca" {
		mitmCA(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "view" {
		view(os.Args[2:])
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

// view serves the archive UI standalone (no proxy), like agentsview serve.
func view(args []string) {
	fs := flag.NewFlagSet("view", flag.ExitOnError)
	db := fs.String("db", "/var/lib/agentfw/view.db", "archive database path")
	addr := fs.String("addr", ":8081", "listen address")
	fs.Parse(args)

	archive, err := agentfw.OpenArchive(*db)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentfw view:", err)
		os.Exit(1)
	}
	defer archive.Close() //nolint:errcheck

	viewer := agentfw.NewViewer()
	viewer.SetArchive(archive)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Addr: *addr, Handler: viewer.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx) //nolint:errcheck
	}()

	log.Printf("agentfw view serving %s at http://%s", *db, *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("agentfw view: %v", err)
	}
}
