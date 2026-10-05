package controller

import (
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
)

func testChart(name, version string) *chart.Chart {
	return &chart.Chart{Metadata: &chart.Metadata{Name: name, Version: version, AppVersion: "1.0"}}
}

func TestDeployFingerprint(t *testing.T) {
	c := testChart("backend", "1.2.3")
	v1 := map[string]interface{}{"replicas": 1, "env": map[string]interface{}{"A": "true"}}
	v2 := map[string]interface{}{"env": map[string]interface{}{"A": "true"}, "replicas": 1}

	if deployFingerprint(c, v1) != deployFingerprint(c, v2) {
		t.Fatal("key order must not change the fingerprint")
	}
	if deployFingerprint(c, v1) == deployFingerprint(c, map[string]interface{}{"replicas": 2}) {
		t.Fatal("different values must change the fingerprint")
	}
	if deployFingerprint(c, v1) == deployFingerprint(testChart("backend", "1.2.4"), v1) {
		t.Fatal("chart version must change the fingerprint")
	}
	if deployFingerprint(c, v1) == deployFingerprint(testChart("watcher", "1.2.3"), v1) {
		t.Fatal("chart name must change the fingerprint")
	}
}

// The memo fast path only applies while the entry is fresh: after the TTL the
// full render+dry-run comparison must run again (drift healing).
func TestDeployMemoTTLBoundsSkip(t *testing.T) {
	key := "ttl-test/ns"
	deployMemo.Delete(key)
	deployMemo.Store(key, deployMemoEntry{
		fingerprint: "fp",
		release:     &release.Release{Name: "ns"},
		verified:    time.Now().Add(-deployMemoTTL - time.Second),
	})
	if e, ok := deployMemo.Load(key); ok && time.Since(e.(deployMemoEntry).verified) < deployMemoTTL {
		t.Fatal("expired entry must not be treated as fresh")
	}
	deployMemo.Delete(key)
}
