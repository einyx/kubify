// Command mlflow-exporter polls the agentfw admin /metrics endpoint and
// pushes per-interval deltas of the agentfw_* counters into MLflow as
// experiment metrics. Raw audit events never leave the log pipeline;
// MLflow only receives derived aggregates (blocks, redactions, scans).
package main

import (
	"flag"
	"log"
	"time"

	"github.com/einyx/kubo/internal/mlflowexp"
)

func main() {
	agentfwURL := flag.String("agentfw", "http://agentfw:8081", "agentfw admin endpoint (serves /metrics)")
	mlflowURL := flag.String("mlflow", "http://mlflow:5000", "MLflow base URL")
	experiment := flag.String("experiment", "agentfw-security", "MLflow experiment name")
	runName := flag.String("run-name", "firewall-aggregates", "MLflow run name")
	interval := flag.Duration("interval", 5*time.Minute, "push interval")
	flag.Parse()

	client := mlflowexp.NewClient(*mlflowURL, *experiment, *runName)
	poller := mlflowexp.NewPoller(*agentfwURL)

	log.Printf("mlflow-exporter: %s -> %s (experiment %q, every %s)",
		*agentfwURL, *mlflowURL, *experiment, *interval)

	var prev map[string]float64
	for range time.Tick(*interval) {
		cur, err := poller.Fetch()
		if err != nil {
			log.Printf("mlflow-exporter: fetch: %v", err)
			continue
		}
		deltas := mlflowexp.Diff(prev, cur)
		prev = cur
		if err := client.Push(deltas); err != nil {
			log.Printf("mlflow-exporter: push: %v", err)
			continue
		}
		if len(deltas) > 0 {
			log.Printf("mlflow-exporter: pushed %d metric deltas", len(deltas))
		}
	}
}
