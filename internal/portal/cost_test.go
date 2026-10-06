package portal

import (
	"math"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEstimateAzureComputeUsesDominantResourceShare(t *testing.T) {
	// Half a D8as_v6 by CPU, but only one quarter by memory.
	got := estimateAzureCompute(4000, 8*1024*1024*1024)
	want := 0.5 * azureNodeHourlyUSD * monthlyHours
	if math.Abs(got-want) > 0.001 {
		t.Fatalf("estimate = %.3f, want %.3f", got, want)
	}
}

func TestNamespaceRequestsCountsSchedulingRequest(t *testing.T) {
	pods := []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("256Mi")}}},
				{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("256Mi")}}},
			},
			InitContainers: []corev1.Container{{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("128Mi")}}}},
		},
	}}
	r := namespaceRequests(pods)["tenant-a"]
	if r.cpuMilli != 1000 || r.memoryBytes != 512*1024*1024 {
		t.Fatalf("request = %+v", r)
	}
}
