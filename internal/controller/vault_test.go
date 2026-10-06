package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
)

func TestReinitSealedVaultWipesRaftOlderThanVaultCR(t *testing.T) {
	now := time.Now()
	cr := vaultTestCR("tenant", now)
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:              "vault-file-vault-0",
		Namespace:         "tenant",
		CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)),
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "vault-0", Namespace: "tenant"}}
	r, c := vaultTestReconciler(t, cr, pvc, pod)

	wiped, err := r.reinitSealedVault(context.Background(), &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant"},
	})
	if err != nil || !wiped {
		t.Fatalf("reinitSealedVault() = (%v, %v), want (true, nil)", wiped, err)
	}
	for _, obj := range []client.Object{pvc, pod} {
		err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
		if !apierrors.IsNotFound(err) {
			t.Errorf("%T was not deleted: %v", obj, err)
		}
	}
}

func TestReinitSealedVaultLeavesFreshRaftAndHonorsOptOut(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name       string
		pvcCreated time.Time
		seed       *platformv1alpha1.VaultSeed
	}{
		{name: "fresh raft", pvcCreated: now.Add(time.Minute)},
		{name: "opted out", pvcCreated: now.Add(-time.Hour), seed: &platformv1alpha1.VaultSeed{AutoReinit: vaultBoolPtr(false)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr := vaultTestCR("tenant", now)
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				Name: "vault-file-vault-0", Namespace: "tenant", CreationTimestamp: metav1.NewTime(tc.pvcCreated),
			}}
			r, c := vaultTestReconciler(t, cr, pvc)
			wiped, err := r.reinitSealedVault(context.Background(), &platformv1alpha1.Stack{
				ObjectMeta: metav1.ObjectMeta{Namespace: "tenant"}, Spec: platformv1alpha1.StackSpec{SeedVault: tc.seed},
			})
			if err != nil || wiped {
				t.Fatalf("reinitSealedVault() = (%v, %v), want (false, nil)", wiped, err)
			}
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: "tenant", Name: pvc.Name}, pvc); err != nil {
				t.Fatalf("PVC should remain: %v", err)
			}
		})
	}
}

func vaultTestCR(namespace string, created time.Time) *unstructured.Unstructured {
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(schema.GroupVersionKind{Group: "vault.banzaicloud.com", Version: "v1alpha1", Kind: "Vault"})
	cr.SetName("vault")
	cr.SetNamespace(namespace)
	cr.SetCreationTimestamp(metav1.NewTime(created))
	return cr
}

func vaultTestReconciler(t *testing.T, objects ...client.Object) (*StackReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &StackReconciler{Client: c, Scheme: scheme}, c
}

func vaultBoolPtr(v bool) *bool { return &v }
