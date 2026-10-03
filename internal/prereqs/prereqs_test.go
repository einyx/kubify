package prereqs

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := apiextensionsv1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return sch
}

func crdExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	return c.Get(t.Context(), types.NamespacedName{Name: name}, crd) == nil
}

func TestEnsureAppliesAllPrereqs(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	if err := Ensure(t.Context(), c); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Spot-check CRDs from both embedded documents landed.
	for _, name := range []string{
		"wasmplugins.extensions.istio.io",          // istio
		"gitrepositories.source.toolkit.fluxcd.io", // flux
	} {
		if !crdExists(t, c, name) {
			t.Errorf("prereq CRD %s not found after Ensure", name)
		}
	}
}

func TestEnsureIdempotent(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	if err := Ensure(t.Context(), c); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	// Second pass must converge without error (server-side apply is a patch).
	if err := Ensure(t.Context(), c); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
}

func TestApplyAllSkipsEmptyDocs(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	if err := applyAll(t.Context(), c, []byte("")); err != nil {
		t.Errorf("empty doc: %v", err)
	}
	if err := applyAll(t.Context(), c, []byte("---\n---\n")); err != nil {
		t.Errorf("blank doc: %v", err)
	}
}

func TestApplyAllBadYAML(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	if err := applyAll(t.Context(), c, []byte("::::\n\t:::")); err == nil {
		t.Error("expected decode error for garbage input")
	}
}
