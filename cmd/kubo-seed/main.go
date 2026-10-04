// Command kubo-seed consolidates kubo-system secret lifecycle.
//
// Usage:
//
//	kubo-seed adopt                     recreate missing sources from tenant copies
//	kubo-seed export -out secrets.yaml  bundle all kubo-system secrets to a file
//	kubo-seed import -in secrets.yaml   restore a bundle (create or update)
//
// The export file is plaintext — encrypt it (age/gpg) before storing it
// anywhere shared. Shared secrets (ghcr-pull-secret, acr-pull-secret,
// meshxregistry-helm-secret) can only come from export/import or manual
// creation; they are never adopted from tenant namespaces.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/einyx/kubo/internal/seed"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/yaml"
)

func main() {
	out := flag.String("out", "kubo-system-secrets.yaml", "export bundle file")
	in := flag.String("in", "kubo-system-secrets.yaml", "import bundle file")
	jsonOut := flag.Bool("json", false, "machine-readable output")
	flag.Parse()

	c, err := client.New(config.GetConfigOrDie(), client.Options{Scheme: seed.Scheme()})
	if err != nil {
		fatal(err)
	}
	ctx := context.Background()

	switch flag.Arg(0) {
	case "adopt":
		res, err := seed.AdoptAll(ctx, c)
		if err != nil {
			fatal(err)
		}
		keys := make([]string, 0, len(res))
		for k := range res {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			r := res[k]
			if *jsonOut {
				json.NewEncoder(os.Stdout).Encode(map[string]any{"stack": k, "adopted": r.Adopted, "missing": r.Missing, "shared": r.Shared})
				continue
			}
			fmt.Printf("%s\n", k)
			for _, a := range r.Adopted {
				fmt.Printf("  adopted  %s\n", a)
			}
			for _, m := range r.Missing {
				fmt.Printf("  MISSING  %s (no tenant copy — provision externally)\n", m)
			}
			for _, s := range r.Shared {
				fmt.Printf("  shared   %s (use import to restore)\n", s)
			}
		}

	case "export":
		b, err := seed.Export(ctx, c)
		if err != nil {
			fatal(err)
		}
		data, err := yaml.Marshal(b)
		if err != nil {
			fatal(err)
		}
		if err := os.WriteFile(*out, data, 0o600); err != nil {
			fatal(err)
		}
		fmt.Printf("exported %d secrets to %s (plaintext — encrypt before sharing)\n", len(b.Secrets), *out)

	case "import":
		data, err := os.ReadFile(*in)
		if err != nil {
			fatal(err)
		}
		var b seed.SecretBundle
		if err := yaml.Unmarshal(data, &b); err != nil {
			fatal(err)
		}
		created, updated, err := seed.Import(ctx, c, &b)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("imported: %d created, %d updated\n", created, updated)

	default:
		fmt.Fprintln(os.Stderr, "usage: kubo-seed {adopt|export|import} [-out f] [-in f]")
		os.Exit(2)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "kubo-seed:", err)
	os.Exit(1)
}
