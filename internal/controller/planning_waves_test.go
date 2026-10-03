package controller

import (
	"fmt"
	"testing"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

func spec(name string, deps ...string) platformv1alpha1.StackComponentSpec {
	return platformv1alpha1.StackComponentSpec{Name: name, DependsOn: deps}
}

func TestDeployWaves(t *testing.T) {
	// postgres <- kafka, backend <- frontend, watcher; frontend <- dai
	byName := map[string]platformv1alpha1.StackComponentSpec{
		"postgres": spec("postgres"),
		"kafka":    spec("kafka"),
		"backend":  spec("backend", "postgres", "kafka"),
		"frontend": spec("frontend", "backend"),
		"watcher":  spec("watcher", "backend"),
		"dai":      spec("dai", "frontend"),
	}
	order := []string{"postgres", "kafka", "backend", "frontend", "watcher", "dai"}

	waves, err := deployWaves(order, byName)
	if err != nil {
		t.Fatalf("deployWaves: %v", err)
	}

	want := [][]string{
		{"postgres", "kafka"},
		{"backend"},
		{"frontend", "watcher"},
		{"dai"},
	}
	if len(waves) != len(want) {
		t.Fatalf("got %d waves, want %d: %v", len(waves), len(want), waves)
	}
	for i, w := range want {
		if len(waves[i]) != len(w) {
			t.Fatalf("wave %d = %v, want %v", i, waves[i], w)
		}
		got := map[string]bool{}
		for _, n := range waves[i] {
			got[n] = true
		}
		for _, n := range w {
			if !got[n] {
				t.Fatalf("wave %d = %v, missing %q", i, waves[i], n)
			}
		}
		// Every dep must be in a strictly earlier wave.
		for _, n := range waves[i] {
			for _, dep := range byName[n].DependsOn {
				if got[dep] {
					t.Errorf("wave %d: %q depends on %q in the same wave", i, n, dep)
				}
			}
		}
	}
}

func TestDeployWavesSingleWave(t *testing.T) {
	byName := map[string]platformv1alpha1.StackComponentSpec{
		"a": spec("a"),
		"b": spec("b"),
	}
	waves, err := deployWaves([]string{"a", "b"}, byName)
	if err != nil {
		t.Fatalf("deployWaves: %v", err)
	}
	if len(waves) != 1 || len(waves[0]) != 2 {
		t.Fatalf("expected one wave with both components, got %v", waves)
	}
}

func TestDeployWavesLarge(t *testing.T) {
	// Chain of 50 components must produce 50 waves and stay topologically
	// consistent.
	byName := map[string]platformv1alpha1.StackComponentSpec{}
	var order []string
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("c%d", i)
		var deps []string
		if i > 0 {
			deps = []string{fmt.Sprintf("c%d", i-1)}
		}
		byName[name] = spec(name, deps...)
		order = append(order, name)
	}
	waves, err := deployWaves(order, byName)
	if err != nil {
		t.Fatalf("deployWaves: %v", err)
	}
	if len(waves) != 50 {
		t.Fatalf("expected 50 waves, got %d", len(waves))
	}
}
