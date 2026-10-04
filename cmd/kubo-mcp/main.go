// Command kubo-mcp runs the kubo Stack Platform MCP server over stdio for
// local AI agents (Claude Code, Cursor, ...). Tools are backed by the
// current kubeconfig — same access, same trust, same RBAC as kubectl.
//
// Register with Claude Code:
//
//	claude mcp add kubo -- kubo-mcp
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/einyx/kubo/internal/mcpserver"
	"github.com/einyx/kubo/internal/portal"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		os.Exit(1)
	}
	if err := corev1.AddToScheme(sch); err != nil {
		os.Exit(1)
	}

	cfg := ctrl.GetConfigOrDie()
	c, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		os.Exit(1)
	}

	pt := portal.New(c)
	pt.EnableOutOfCluster(cfg)
	srv := mcpserver.New(c, "")
	srv.Portal = pt

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := mcpserver.ServeStdioDefault(ctx, srv); err != nil {
		os.Exit(1)
	}
}
