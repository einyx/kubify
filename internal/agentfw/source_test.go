package agentfw

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSourceResolverResolvesAndCachesPodIdentity(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "backend-abc", Namespace: "integration"},
		Status:     corev1.PodStatus{PodIP: "10.0.0.12", PodIPs: []corev1.PodIP{{IP: "10.0.0.12"}}},
	})
	resolver := &SourceResolver{client: client, namespace: "integration", cache: map[string]string{}}

	ip, name := resolver.Resolve("10.0.0.12:45678")
	if ip != "10.0.0.12" || name != "integration/backend-abc" {
		t.Fatalf("Resolve() = %q, %q", ip, name)
	}
	if err := client.CoreV1().Pods("integration").Delete(t.Context(), "backend-abc", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, cached := resolver.Resolve("10.0.0.12:45679"); cached != name {
		t.Fatalf("cached identity = %q, want %q", cached, name)
	}
}

func TestSourceIP(t *testing.T) {
	for input, want := range map[string]string{
		"10.0.0.1:8080":     "10.0.0.1",
		"[2001:db8::1]:443": "2001:db8::1",
		"10.0.0.2":          "10.0.0.2",
	} {
		if got := sourceIP(input); got != want {
			t.Errorf("sourceIP(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestSourceResolverRefreshesUnknownIPs(t *testing.T) {
	client := fake.NewSimpleClientset()
	resolver := &SourceResolver{client: client, namespace: "integration", cache: map[string]string{}}
	if _, name := resolver.Resolve("10.0.0.20:1234"); name != "" {
		t.Fatalf("unexpected identity %q", name)
	}
	_, err := client.CoreV1().Pods("integration").Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "new-pod", Namespace: "integration"},
		Status:     corev1.PodStatus{PodIP: "10.0.0.21"},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, name := resolver.Resolve("10.0.0.21:5678"); name != "integration/new-pod" {
		t.Fatalf("refreshed identity = %q", name)
	}
}
