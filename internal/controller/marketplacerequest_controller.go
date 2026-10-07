package controller

import (
	"context"
	"fmt"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// MarketplaceRequestReconciler converts activated purchases into approved
// DemoRequests so Marketplace tenants use the established provisioning path.
type MarketplaceRequestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=platform.kubo.io,resources=marketplacerequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.kubo.io,resources=marketplacerequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.kubo.io,resources=demorequests,verbs=get;list;watch;create

func (r *MarketplaceRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var mr platformv1alpha1.MarketplaceRequest
	if err := r.Get(ctx, req.NamespacedName, &mr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if mr.Spec.Provider != "azure" {
		return ctrl.Result{}, r.setStatus(ctx, &mr, platformv1alpha1.MarketplaceRequestFailed, "unsupported marketplace provider")
	}

	demoName := mr.Name
	var dr platformv1alpha1.DemoRequest
	err := r.Get(ctx, types.NamespacedName{Namespace: mr.Namespace, Name: demoName}, &dr)
	if apierrors.IsNotFound(err) {
		permanent := metav1.Duration{}
		dr = platformv1alpha1.DemoRequest{
			ObjectMeta: metav1.ObjectMeta{Namespace: mr.Namespace, Name: demoName},
			Spec: platformv1alpha1.DemoRequestSpec{
				Email: mr.Spec.PurchaserEmail, Company: mr.Spec.SubscriptionName,
				Template: mr.Spec.Template, Approved: true, TTL: &permanent, SkipNotification: true,
			},
		}
		if err := controllerutil.SetControllerReference(&mr, &dr, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, &dr); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.setStatus(ctx, &mr, platformv1alpha1.MarketplaceRequestProvisioning, "Foundation tenant request created")
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	phase := platformv1alpha1.MarketplaceRequestProvisioning
	if dr.Status.Phase == platformv1alpha1.DemoRequestReady {
		phase = platformv1alpha1.MarketplaceRequestReady
	}
	if dr.Status.Phase == platformv1alpha1.DemoRequestFailed {
		phase = platformv1alpha1.MarketplaceRequestFailed
	}
	mr.Status.DemoRequestRef = dr.Name
	mr.Status.Tenant = dr.Status.Tenant
	mr.Status.URL = dr.Status.URL
	mr.Status.Message = dr.Status.Message
	mr.Status.Phase = phase
	mr.Status.ObservedGeneration = mr.Generation
	if err := r.Status().Update(ctx, &mr); err != nil {
		return ctrl.Result{}, fmt.Errorf("update MarketplaceRequest status: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *MarketplaceRequestReconciler) setStatus(ctx context.Context, mr *platformv1alpha1.MarketplaceRequest, phase platformv1alpha1.MarketplaceRequestPhase, message string) error {
	mr.Status.Phase, mr.Status.Message, mr.Status.ObservedGeneration = phase, message, mr.Generation
	return r.Status().Update(ctx, mr)
}

func (r *MarketplaceRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&platformv1alpha1.MarketplaceRequest{}).Owns(&platformv1alpha1.DemoRequest{}).Complete(r)
}
