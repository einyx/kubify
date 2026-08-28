package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

// fakeHelm records Deploy calls and can fail selected components.
type fakeHelm struct {
	mu        sync.Mutex
	deployed  []string // in call order, "ns/name"
	fail      map[string]error
	charts    map[string]*chart.Chart
	revision  int
	deploySem chan struct{} // if set, held while deploying (for concurrency tests)
}

func (f *fakeHelm) EnsureChart(ref platformv1alpha1.ChartRef, _, _ string) (*chart.Chart, error) {
	if err, ok := f.fail["chart:"+ref.ChartName]; ok {
		return nil, err
	}
	if ch, ok := f.charts[ref.ChartName]; ok {
		return ch, nil
	}
	return &chart.Chart{Metadata: &chart.Metadata{Name: ref.ChartName, Version: "0.1.0"}}, nil
}

func (f *fakeHelm) Deploy(compName, namespace string, _ *chart.Chart, _ map[string]interface{}) (*release.Release, error) {
	if f.deploySem != nil {
		f.deploySem <- struct{}{}
		defer func() { <-f.deploySem }()
		time.Sleep(10 * time.Millisecond) // make overlap observable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.fail[compName]; ok {
		f.deployed = append(f.deployed, "FAIL:"+namespace+"/"+compName)
		return nil, err
	}
	f.deployed = append(f.deployed, namespace+"/"+compName)
	f.revision++
	return &release.Release{
		Name:    compName,
		Version: f.revision,
		Info:    &release.Info{Description: "ok"},
	}, nil
}

func (f *fakeHelm) Uninstall(name, namespace string) error { return nil }

func (f *fakeHelm) ReleaseStatus(name, namespace string) (release.Status, error) {
	return release.StatusDeployed, nil
}

func (f *fakeHelm) order() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deployed...)
}

func deployScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return sch
}

func stackFor(ns string) *platformv1alpha1.Stack {
	return &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Name: "st", Namespace: ns, Generation: 1},
		Spec:       platformv1alpha1.StackSpec{Mode: platformv1alpha1.DeploymentModeDirect},
	}
}

func componentList(byName map[string]platformv1alpha1.StackComponentSpec) []platformv1alpha1.StackComponentSpec {
	out := make([]platformv1alpha1.StackComponentSpec, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	return out
}

func TestDeployDirectParallelWaves(t *testing.T) {
	// postgres + kafka are independent (same wave); backend depends on both;
	// frontend + watcher depend on backend.
	byName := map[string]platformv1alpha1.StackComponentSpec{
		"postgres": {Name: "postgres"},
		"kafka":    {Name: "kafka"},
		"backend":  {Name: "backend", DependsOn: []string{"postgres", "kafka"}},
		"frontend": {Name: "frontend", DependsOn: []string{"backend"}},
		"watcher":  {Name: "watcher", DependsOn: []string{"backend"}},
	}
	order, err := topoOrder(componentList(byName))
	if err != nil {
		t.Fatal(err)
	}

	fh := &fakeHelm{deploySem: make(chan struct{}, 2)} // 2 slots -> wave of 2 must overlap
	r := &StackReconciler{Helm: fh}
	start := time.Now()
	statuses, err := r.deployDirect(context.Background(), stackFor("ns"), order, byName)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}

	// 5 components x >=10ms each; sequential would be >=50ms, parallel waves ~20ms.
	if elapsed > 45*time.Millisecond {
		t.Errorf("deployment looks sequential: %v for %d components", elapsed, len(statuses))
	}
	got := map[string]bool{}
	for _, s := range statuses {
		if s.Phase != platformv1alpha1.ComponentPhaseReady {
			t.Errorf("%s phase = %q", s.Name, s.Phase)
		}
		got[s.Name] = true
	}
	for _, n := range []string{"postgres", "kafka", "backend", "frontend", "watcher"} {
		if !got[n] {
			t.Errorf("component %s missing from statuses", n)
		}
	}
	// Dependencies must still be honored in call order.
	pos := map[string]int{}
	for i, d := range fh.order() {
		pos[strings.SplitN(d, "/", 2)[1]] = i
	}
	for _, pair := range [][2]string{{"postgres", "backend"}, {"kafka", "backend"}, {"backend", "frontend"}, {"backend", "watcher"}} {
		if pos[pair[0]] >= pos[pair[1]] {
			t.Errorf("%s deployed before its dependency %s", pair[1], pair[0])
		}
	}
}

func TestDeployDirectSkipsDependentsOfFailed(t *testing.T) {
	byName := map[string]platformv1alpha1.StackComponentSpec{
		"postgres":  {Name: "postgres"},
		"backend":   {Name: "backend", DependsOn: []string{"postgres"}},
		"frontend":  {Name: "frontend", DependsOn: []string{"backend"}},
		"unrelated": {Name: "unrelated"},
	}
	order, _ := topoOrder(componentList(byName))
	fh := &fakeHelm{fail: map[string]error{"postgres": fmt.Errorf("boom")}}
	r := &StackReconciler{Helm: fh}

	statuses, err := r.deployDirect(context.Background(), stackFor("ns"), order, byName)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want firstErr boom, got %v", err)
	}
	bySt := map[string]platformv1alpha1.ComponentStatus{}
	for _, s := range statuses {
		bySt[s.Name] = s
	}
	if bySt["postgres"].Phase != platformv1alpha1.ComponentPhaseFailed {
		t.Errorf("postgres = %q", bySt["postgres"].Phase)
	}
	if bySt["backend"].Phase != platformv1alpha1.ComponentPhasePending ||
		!strings.Contains(bySt["backend"].Message, `dependency "postgres" failed`) {
		t.Errorf("backend = %+v", bySt["backend"])
	}
	if bySt["frontend"].Phase != platformv1alpha1.ComponentPhasePending {
		t.Errorf("transitive dependent should be skipped, got %+v", bySt["frontend"])
	}
	if bySt["unrelated"].Phase != platformv1alpha1.ComponentPhaseReady {
		t.Errorf("unrelated component should still deploy, got %+v", bySt["unrelated"])
	}
	for _, d := range fh.order() {
		if strings.HasSuffix(d, "/backend") || strings.HasSuffix(d, "/frontend") {
			t.Errorf("skipped component was deployed: %s", d)
		}
	}
}

func TestDeployComponentStatusMapping(t *testing.T) {
	fh := &fakeHelm{fail: map[string]error{"bad": fmt.Errorf("install failed")}}
	r := &StackReconciler{Helm: fh}

	st, err := r.deployComponent(context.Background(), stackFor("ns"),
		platformv1alpha1.StackComponentSpec{Name: "good"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != platformv1alpha1.ComponentPhaseReady || st.Revision == 0 || st.Message != "ok" {
		t.Errorf("good status = %+v", st)
	}

	st, err = r.deployComponent(context.Background(), stackFor("ns"),
		platformv1alpha1.StackComponentSpec{Name: "bad"})
	if err == nil {
		t.Fatal("want error")
	}
	if st.Phase != platformv1alpha1.ComponentPhaseFailed || !strings.Contains(st.Message, "install failed") {
		t.Errorf("bad status = %+v", st)
	}

	// chart pull failure surfaces as Failed too
	st, err = r.deployComponent(context.Background(), stackFor("ns"),
		platformv1alpha1.StackComponentSpec{Name: "worse", ChartRef: platformv1alpha1.ChartRef{ChartName: "chart-fail"}})
	_ = st
	if err == nil {
		// only fails when the fake is told to; the "chart:" prefix failure key
		// applies to ChartName
	}
	fh.fail["chart:chart-fail"] = fmt.Errorf("pull exploded")
	st, err = r.deployComponent(context.Background(), stackFor("ns"),
		platformv1alpha1.StackComponentSpec{Name: "worse", ChartRef: platformv1alpha1.ChartRef{ChartName: "chart-fail"}})
	if err == nil || st.Phase != platformv1alpha1.ComponentPhaseFailed {
		t.Errorf("chart pull failure not surfaced: %+v %v", st, err)
	}
}
