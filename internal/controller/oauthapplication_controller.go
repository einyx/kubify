package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const oauthApplicationFinalizer = "platform.kubo.io/oauth-application"

// +kubebuilder:rbac:groups=platform.kubo.io,resources=oauthapplications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.kubo.io,resources=oauthapplications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.kubo.io,resources=oauthapplications/finalizers,verbs=update
// +kubebuilder:rbac:groups=platform.kubo.io,resources=oauthproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch
type OAuthApplicationReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	HTTPClient *http.Client
}

func (r *OAuthApplicationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var app platformv1alpha1.OAuthApplication
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if app.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(&app, oauthApplicationFinalizer) {
		controllerutil.AddFinalizer(&app, oauthApplicationFinalizer)
		if err := r.Update(ctx, &app); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if !app.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(&app, oauthApplicationFinalizer) {
		return ctrl.Result{}, nil
	}
	provider, remote, err := r.auth0(ctx, &app)
	if err != nil {
		return r.failed(ctx, &app, "ProviderUnavailable", err)
	}
	if !app.DeletionTimestamp.IsZero() {
		if app.Status.ProviderApplicationID != "" {
			if err := remote.delete(ctx, app.Status.ProviderApplicationID); err != nil && !strings.Contains(err.Error(), "404") {
				return ctrl.Result{}, err
			}
		}
		controllerutil.RemoveFinalizer(&app, oauthApplicationFinalizer)
		return ctrl.Result{}, r.Update(ctx, &app)
	}
	desired := auth0Application{Name: app.Namespace + "-" + app.Name, AppType: app.Spec.ApplicationType, Callbacks: app.Spec.Callbacks, LogoutURLs: app.Spec.LogoutURLs, WebOrigins: app.Spec.WebOrigins, AllowedOrigins: app.Spec.AllowedOrigins, Metadata: map[string]string{"managed-by": "kubo", "kubo-namespace": app.Namespace, "kubo-name": app.Name, "kubo-uid": string(app.UID)}}
	secretValue := ""
	if app.Status.ProviderApplicationID == "" {
		created, e := remote.create(ctx, desired)
		if e != nil {
			return r.failed(ctx, &app, "CreateFailed", e)
		}
		app.Status.ProviderApplicationID = created.ID
		secretValue = created.Secret
	} else if err := remote.update(ctx, app.Status.ProviderApplicationID, desired); err != nil {
		return r.failed(ctx, &app, "UpdateFailed", err)
	}
	var target corev1.Secret
	secretKey := types.NamespacedName{Namespace: app.Namespace, Name: app.Spec.SecretTargetRef.Name}
	if err := r.Get(ctx, secretKey, &target); apierrors.IsNotFound(err) {
		if secretValue == "" {
			secretValue, err = remote.rotate(ctx, app.Status.ProviderApplicationID)
			if err != nil {
				return r.failed(ctx, &app, "SecretRotationFailed", err)
			}
		}
		target = corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: secretKey.Namespace}, Type: corev1.SecretTypeOpaque}
		if err := controllerutil.SetControllerReference(&app, &target, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		target.Data = map[string][]byte{"client-id": []byte(app.Status.ProviderApplicationID), "client-secret": []byte(secretValue), "issuer": []byte(provider.Spec.Issuer)}
		if err := r.Create(ctx, &target); err != nil {
			return ctrl.Result{}, err
		}
	} else if err != nil {
		return ctrl.Result{}, err
	} else {
		changed := string(target.Data["client-id"]) != app.Status.ProviderApplicationID || string(target.Data["issuer"]) != provider.Spec.Issuer
		if len(target.Data["client-secret"]) == 0 {
			secretValue, err = remote.rotate(ctx, app.Status.ProviderApplicationID)
			if err != nil {
				return r.failed(ctx, &app, "SecretRotationFailed", err)
			}
			target.Data["client-secret"] = []byte(secretValue)
			changed = true
		}
		target.Data["client-id"] = []byte(app.Status.ProviderApplicationID)
		target.Data["issuer"] = []byte(provider.Spec.Issuer)
		if changed {
			if err := r.Update(ctx, &target); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	app.Status.ObservedGeneration = app.Generation
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Reconciled", Message: "OAuth application and credential Secret are ready", ObservedGeneration: app.Generation})
	return ctrl.Result{RequeueAfter: 30 * time.Minute}, r.Status().Update(ctx, &app)
}

func (r *OAuthApplicationReconciler) auth0(ctx context.Context, app *platformv1alpha1.OAuthApplication) (*platformv1alpha1.OAuthProvider, *auth0Client, error) {
	var p platformv1alpha1.OAuthProvider
	if err := r.Get(ctx, types.NamespacedName{Name: app.Spec.ProviderRef}, &p); err != nil {
		return nil, nil, err
	}
	if p.Spec.Type != "Auth0" {
		return nil, nil, fmt.Errorf("unsupported provider type %q", p.Spec.Type)
	}
	ref := p.Spec.ManagementCredentialsSecretRef
	var s corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &s); err != nil {
		return nil, nil, err
	}
	id, secret := string(s.Data["client-id"]), string(s.Data["client-secret"])
	if id == "" || secret == "" {
		return nil, nil, fmt.Errorf("management Secret must contain client-id and client-secret")
	}
	apiURL := p.Spec.ManagementAPIURL
	audience := p.Spec.ManagementAudience
	if audience == "" {
		audience = strings.TrimRight(apiURL, "/") + "/"
	}
	tokenURL := p.Spec.TokenURL
	if tokenURL == "" {
		tokenURL = strings.TrimRight(p.Spec.Issuer, "/") + "/oauth/token"
	}
	hc := r.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &p, &auth0Client{http: hc, tokenURL: tokenURL, audience: audience, apiURL: apiURL, clientID: id, clientSecret: secret}, nil
}

func (r *OAuthApplicationReconciler) failed(ctx context.Context, app *platformv1alpha1.OAuthApplication, reason string, err error) (ctrl.Result, error) {
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: reason, Message: err.Error(), ObservedGeneration: app.Generation})
	_ = r.Status().Update(ctx, app)
	return ctrl.Result{}, err
}
func (r *OAuthApplicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&platformv1alpha1.OAuthApplication{}).Owns(&corev1.Secret{}).Complete(r)
}
