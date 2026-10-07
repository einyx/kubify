package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestOAuthApplicationReconcilesAuth0ClientAndSecret(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token", "expires_in": 3600})
		case "/api/v2/clients":
			_ = json.NewEncoder(w).Encode(auth0Application{ID: "auth0-client-id", Secret: "auth0-client-secret"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	provider := &platformv1alpha1.OAuthProvider{ObjectMeta: metav1.ObjectMeta{Name: "auth0"}, Spec: platformv1alpha1.OAuthProviderSpec{
		Type: "Auth0", Issuer: server.URL, ManagementAPIURL: server.URL + "/api/v2",
		ManagementCredentialsSecretRef: platformv1alpha1.OAuthSecretReference{Namespace: "kubo-system", Name: "auth0-management"},
	}}
	management := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "kubo-system", Name: "auth0-management"}, Data: map[string][]byte{"client-id": []byte("manager"), "client-secret": []byte("manager-secret")}}
	app := &platformv1alpha1.OAuthApplication{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "frontend", UID: types.UID("uid-1")}, Spec: platformv1alpha1.OAuthApplicationSpec{ProviderRef: "auth0", ApplicationType: "regular_web", SecretTargetRef: platformv1alpha1.OAuthApplicationSecretTargetReference{Name: "frontend-auth"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(app).WithObjects(provider, management, app).Build()
	r := &OAuthApplicationReconciler{Client: c, Scheme: scheme, HTTPClient: server.Client()}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: app.Namespace, Name: app.Name}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	} // finalizer
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	} // provision
	var target corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: "frontend-auth"}, &target); err != nil {
		t.Fatal(err)
	}
	if got := string(target.Data["client-secret"]); got != "auth0-client-secret" {
		t.Fatalf("client-secret = %q", got)
	}
	if got := string(target.Data["issuer"]); got != server.URL {
		t.Fatalf("issuer = %q", got)
	}
	var reconciled platformv1alpha1.OAuthApplication
	if err := c.Get(context.Background(), req.NamespacedName, &reconciled); err != nil {
		t.Fatal(err)
	}
	if reconciled.Status.ProviderApplicationID != "auth0-client-id" {
		t.Fatalf("provider id = %q", reconciled.Status.ProviderApplicationID)
	}
}
