package seed

import (
	"context"
	"testing"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(sch); err != nil {
		t.Fatal(err)
	}
	return sch
}

func secret(ns, name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"k": []byte("v-" + ns + "-" + name)}}
}

func testStack() *platformv1alpha1.Stack {
	return &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Name: "foundation", Namespace: "foundation-x"},
		Spec: platformv1alpha1.StackSpec{
			SecretsRef: []platformv1alpha1.SecretMapping{
				{From: "foundation-x-backend-auth0", To: "backend-auth0"},
				{From: "foundation-x-ai-secrets", To: "ai-secrets"},
			},
			Inline: &platformv1alpha1.StackDefinitionSpec{
				Components: []platformv1alpha1.StackComponentSpec{{
					ChartPullSecretRef: &corev1.LocalObjectReference{Name: "ghcr-pull-secret"},
				}},
			},
		},
	}
}

func TestAdoptAdoptsTenantPrefixedOnly(t *testing.T) {
	sch := scheme(t)
	// Tenant has backend-auth0 but not ai-secrets; ghcr-pull-secret (shared)
	// also present in the tenant — must NOT be adopted.
	objs := []client.Object{
		testStack(), secret("foundation-x", "backend-auth0"), secret("foundation-x", "ghcr-pull-secret"),
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()

	res, err := AdoptStack(context.Background(), c, testStack())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 1 || res.Adopted[0] != "foundation-x-backend-auth0" {
		t.Fatalf("adopted: %v", res.Adopted)
	}

	var src corev1.Secret
	if err := c.Get(context.Background(),
		client.ObjectKey{Namespace: "kubo-system", Name: "foundation-x-backend-auth0"}, &src); err != nil {
		t.Fatal("adopted source missing")
	}
	if string(src.Data["k"]) != "v-foundation-x-backend-auth0" {
		t.Fatalf("adopted data wrong: %q", src.Data["k"])
	}
	if src.Annotations["platform.kubo.io/adopted-from"] != "foundation-x/backend-auth0" {
		t.Fatal("adoption annotation missing")
	}

	// Shared secret must never be adopted from a tenant.
	if err := c.Get(context.Background(),
		client.ObjectKey{Namespace: "kubo-system", Name: "ghcr-pull-secret"}, &corev1.Secret{}); err == nil {
		t.Fatal("shared secret was adopted from tenant — privilege escalation")
	}
	if len(res.Shared) != 1 || res.Shared[0] != "ghcr-pull-secret" {
		t.Fatalf("shared reporting: %v", res.Shared)
	}
	if len(res.Missing) != 1 || res.Missing[0] != "foundation-x-ai-secrets" {
		t.Fatalf("missing reporting: %v", res.Missing)
	}

	// Missing-without-tenant-copy stays missing after a second pass.
	res2, err := AdoptStack(context.Background(), c, testStack())
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Adopted) != 0 {
		t.Fatalf("second pass re-adopted: %v", res2.Adopted)
	}
}

func TestAdoptPresentSourceUntouched(t *testing.T) {
	sch := scheme(t)
	src := secret("kubo-system", "foundation-x-backend-auth0")
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(testStack(), src,
		secret("foundation-x", "backend-auth0")).Build()

	res, err := AdoptStack(context.Background(), c, testStack())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 0 {
		t.Fatalf("adopted despite present source: %v", res.Adopted)
	}
	var after corev1.Secret
	c.Get(context.Background(), client.ObjectKey{Namespace: "kubo-system", Name: "foundation-x-backend-auth0"}, &after)
	if string(after.Data["k"]) != "v-kubo-system-foundation-x-backend-auth0" {
		t.Fatal("existing source was overwritten by adoption pass")
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	sch := scheme(t)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		secret("kubo-system", "ghcr-pull-secret"),
		secret("kubo-system", "foundation-x-backend-auth0"),
	).Build()

	b, err := Export(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Secrets) != 2 {
		t.Fatalf("exported %d", len(b.Secrets))
	}

	// Fresh cluster: import restores both.
	c2 := fake.NewClientBuilder().WithScheme(sch).Build()
	created, updated, err := Import(context.Background(), c2, b)
	if err != nil {
		t.Fatal(err)
	}
	if created != 2 || updated != 0 {
		t.Fatalf("import: created=%d updated=%d", created, updated)
	}
	// Re-import updates in place.
	created, updated, err = Import(context.Background(), c2, b)
	if err != nil || created != 0 || updated != 2 {
		t.Fatalf("re-import: created=%d updated=%d err=%v", created, updated, err)
	}
}
