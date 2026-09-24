package controller

import (
	"context"
	"testing"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPublishStackLifecyclePhase(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	stack := &platformv1alpha1.Stack{ObjectMeta: metav1.ObjectMeta{Name: "tenant", Namespace: "tenant", Generation: 2}}
	stack.Status.Phase = "Ready"
	stack.Status.ObservedGeneration = 1
	meta.SetStatusCondition(&stack.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AllComponentsReady"})
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(stack).WithObjects(stack).Build()
	r := &StackReconciler{Client: c}
	ctx := context.Background()
	for _, phase := range []string{"Pending", "Progressing", "Paused", "Terminating"} {
		if err := r.publishStackPhase(ctx, stack, phase, phase, "lifecycle transition"); err != nil {
			t.Fatal(err)
		}
		var got platformv1alpha1.Stack
		if err := c.Get(ctx, client.ObjectKeyFromObject(stack), &got); err != nil {
			t.Fatal(err)
		}
		ready := meta.FindStatusCondition(got.Status.Conditions, "Ready")
		if got.Status.Phase != phase || ready == nil || ready.Status != metav1.ConditionFalse || got.Status.ObservedGeneration != 1 {
			t.Fatalf("incorrect persisted transition: %+v", got.Status)
		}
		version := got.ResourceVersion
		if err := r.publishStackPhase(ctx, stack, phase, phase, "lifecycle transition"); err != nil {
			t.Fatal(err)
		}
		if stack.ResourceVersion != version {
			t.Fatal("unchanged status caused another write")
		}
	}
}
