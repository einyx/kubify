package controller

import (
	"context"
	"strings"
	"testing"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func agentfwScheme(t *testing.T) *runtime.Scheme {
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

// A same-count port mutation (renamed / re-targeted port) must be healed —
// count-only comparison let such drift persist forever.
func TestAgentFWServiceHealsSameCountPortDrift(t *testing.T) {
	sch := agentfwScheme(t)
	drifted := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: agentfwName, Namespace: "foundation-x"},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
			{Name: "client", Port: agentfwPort, Protocol: corev1.ProtocolTCP},
			{Name: "admin-renamed", Port: agentfwAdminPort + 1, Protocol: corev1.ProtocolTCP},
		}}}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(drifted).Build()
	r := &StackReconciler{Client: c, Scheme: sch}

	if err := r.ensureAgentFWService(context.Background(), "foundation-x"); err != nil {
		t.Fatal(err)
	}
	var svc corev1.Service
	if err := r.Client.Get(context.Background(), client.ObjectKey{Namespace: "foundation-x", Name: agentfwName}, &svc); err != nil {
		t.Fatal(err)
	}
	if len(svc.Spec.Ports) != 2 || svc.Spec.Ports[1].Name != "admin" || svc.Spec.Ports[1].Port != agentfwAdminPort {
		t.Fatalf("drift not healed: %+v", svc.Spec.Ports)
	}
}

func TestAgentFWServiceNoChurnWhenInSync(t *testing.T) {
	sch := agentfwScheme(t)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: agentfwName, Namespace: "foundation-x", ResourceVersion: "42"},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{
			{Port: agentfwPort, TargetPort: intstr.FromInt(agentfwPort), Protocol: corev1.ProtocolTCP},
			{Name: "admin", Port: agentfwAdminPort, TargetPort: intstr.FromInt(agentfwAdminPort), Protocol: corev1.ProtocolTCP},
		}}}
	r := &StackReconciler{Client: fake.NewClientBuilder().WithScheme(sch).WithObjects(svc).Build(), Scheme: sch}

	if err := r.ensureAgentFWService(context.Background(), "foundation-x"); err != nil {
		t.Fatal(err)
	}
	var after corev1.Service
	if err := r.Client.Get(context.Background(), client.ObjectKey{Namespace: "foundation-x", Name: agentfwName}, &after); err != nil {
		t.Fatal(err)
	}
	if after.ResourceVersion != "42" {
		t.Fatalf("in-sync service was rewritten (resourceVersion %s)", after.ResourceVersion)
	}
}

// The agentfw deployment update must sync volumes alongside containers: a
// container that gains a volumeMount while Volumes stays stale renders an
// invalid Deployment — the exact foundation-a outage.
func TestAgentFWDeploymentUpdateSyncsVolumes(t *testing.T) {
	sch := agentfwScheme(t)
	// Old-shape deployment: only the policy volume, no viewer-archive.
	stale := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: agentfwName, Namespace: "foundation-x"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:         agentfwName,
					Image:        "agentfw:old",
					VolumeMounts: []corev1.VolumeMount{{Name: "policy", MountPath: "/etc/agentfw", ReadOnly: true}},
				}},
				Volumes: []corev1.Volume{{Name: "policy"}},
			}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(stale).Build()
	r := &StackReconciler{Client: c, Scheme: sch}

	if err := r.ensureAgentFWDeployment(context.Background(), "foundation-x"); err != nil {
		t.Fatal(err)
	}

	var after appsv1.Deployment
	if err := r.Client.Get(context.Background(),
		client.ObjectKey{Namespace: "foundation-x", Name: agentfwName}, &after); err != nil {
		t.Fatal(err)
	}
	mounts := map[string]bool{}
	for _, m := range after.Spec.Template.Spec.Containers[0].VolumeMounts {
		mounts[m.Name] = true
	}
	vols := map[string]bool{}
	for _, v := range after.Spec.Template.Spec.Volumes {
		vols[v.Name] = true
		for _, m := range after.Spec.Template.Spec.Containers[0].VolumeMounts {
			if m.Name == v.Name {
				delete(mounts, m.Name) // every mount has a volume
			}
		}
	}
	if len(mounts) > 0 {
		t.Fatalf("mounts without volumes: %v", mounts)
	}
	if !vols["viewer-archive"] {
		t.Fatalf("viewer-archive volume missing after update: %v", vols)
	}
	if !strings.Contains(after.Spec.Template.Spec.Containers[0].Image, "agentfw") {
		t.Fatalf("image not synced: %s", after.Spec.Template.Spec.Containers[0].Image)
	}
}
