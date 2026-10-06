package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	"github.com/einyx/kubo/internal/writeback"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const writebackSecret = "kubo-git-writeback"

// GitWritebackRequestReconciler turns an allowlisted Stack mutation into a PR.
// Git credentials remain in the controller namespace and never reach the portal.
// +kubebuilder:rbac:groups=platform.kubo.io,resources=gitwritebackrequests,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=platform.kubo.io,resources=gitwritebackrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.kubo.io,resources=stacks,verbs=get;list;watch
// +kubebuilder:rbac:groups=kustomize.toolkit.fluxcd.io,resources=kustomizations,verbs=get;list;watch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=gitrepositories,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get
type GitWritebackRequestReconciler struct {
	client.Client
	HTTPClient      *http.Client
	SystemNamespace string
}

func (r *GitWritebackRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var wb platformv1alpha1.GitWritebackRequest
	if err := r.Get(ctx, req.NamespacedName, &wb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if wb.Status.PullRequestURL != "" || wb.Status.Phase == platformv1alpha1.GitWritebackApplied {
		return ctrl.Result{}, nil
	}
	if len(wb.Spec.FeatureFlags) == 0 {
		return r.fail(ctx, &wb, platformv1alpha1.GitWritebackFailed, "featureFlags cannot be empty", nil)
	}
	wb.Status.Phase = platformv1alpha1.GitWritebackRunning
	wb.Status.ObservedGeneration = wb.Generation
	_ = r.Status().Update(ctx, &wb)
	var stack platformv1alpha1.Stack
	if err := r.Get(ctx, types.NamespacedName{Namespace: wb.Spec.Target.Namespace, Name: wb.Spec.Target.Name}, &stack); err != nil {
		return r.fail(ctx, &wb, platformv1alpha1.GitWritebackFailed, "read target Stack", err)
	}
	loc, err := (&writeback.Locator{Client: r.Client, HTTPClient: r.HTTPClient}).Locate(ctx, &stack)
	if err != nil {
		return r.fail(ctx, &wb, platformv1alpha1.GitWritebackFailed, "locate Stack source", err)
	}
	ns := r.SystemNamespace
	if ns == "" {
		ns = "kubo-system"
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: writebackSecret}, &secret); err != nil {
		return r.fail(ctx, &wb, platformv1alpha1.GitWritebackFailed, "read GitHub credential", err)
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		token, err = writeback.InstallationToken(ctx, string(secret.Data["app_id"]), string(secret.Data["installation_id"]), secret.Data["private_key"], r.HTTPClient)
		if err != nil {
			return r.fail(ctx, &wb, platformv1alpha1.GitWritebackFailed, "authenticate GitHub App", err)
		}
	}
	branch := fmt.Sprintf("kubify/%s-%s-%s", stack.Namespace, stack.Name, string(wb.UID)[:8])
	title := fmt.Sprintf("%s/%s: update feature flags", stack.Namespace, stack.Name)
	pr, err := (&writeback.GitHub{Token: token, HTTPClient: r.HTTPClient}).CreateFeatureFlagPR(ctx, loc.Repository, loc.File, stack.Namespace, stack.Name, wb.Spec.ExpectedFeatureFlags, wb.Spec.FeatureFlags, title, branch)
	if err != nil {
		phase := platformv1alpha1.GitWritebackFailed
		if strings.Contains(err.Error(), "changed from expected") {
			phase = platformv1alpha1.GitWritebackConflict
		}
		return r.fail(ctx, &wb, phase, "create pull request", err)
	}
	wb.Status.Phase = platformv1alpha1.GitWritebackPullRequestOpen
	wb.Status.Repository = loc.Repository
	wb.Status.Path = loc.File
	wb.Status.Branch = pr.Branch
	wb.Status.Commit = pr.Commit
	wb.Status.PullRequestURL = pr.URL
	wb.Status.Message = "Pull request opened"
	apimeta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "PullRequestOpen", Message: pr.URL, ObservedGeneration: wb.Generation})
	return ctrl.Result{}, r.Status().Update(ctx, &wb)
}

func (r *GitWritebackRequestReconciler) fail(ctx context.Context, wb *platformv1alpha1.GitWritebackRequest, phase platformv1alpha1.GitWritebackPhase, msg string, err error) (ctrl.Result, error) {
	if err != nil {
		msg += ": " + err.Error()
	}
	wb.Status.Phase = phase
	wb.Status.Message = msg
	wb.Status.ObservedGeneration = wb.Generation
	apimeta.SetStatusCondition(&wb.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: string(phase), Message: msg, ObservedGeneration: wb.Generation})
	if e := r.Status().Update(ctx, wb); e != nil && !apierrors.IsConflict(e) {
		return ctrl.Result{}, e
	}
	log.FromContext(ctx).Error(fmt.Errorf("%s", msg), "Git write-back failed")
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *GitWritebackRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&platformv1alpha1.GitWritebackRequest{}).Complete(r)
}
