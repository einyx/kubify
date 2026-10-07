package agentfw

import (
	"context"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// SourceResolver resolves a connection address while the source pod still
// exists. Persisting this result with the request preserves identity after pod
// replacement, unlike viewer-time IP lookups.
type SourceResolver struct {
	client    kubernetes.Interface
	namespace string
	mu        sync.RWMutex
	cache     map[string]string
	expires   time.Time
}

func NewInClusterSourceResolver() *SourceResolver {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil
	}
	namespaceBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return nil
	}
	return &SourceResolver{client: client, namespace: strings.TrimSpace(string(namespaceBytes)), cache: map[string]string{}}
}

func (r *SourceResolver) Resolve(remoteAddr string) (string, string) {
	ip := sourceIP(remoteAddr)
	if r == nil || ip == "" {
		return ip, ""
	}
	r.mu.RLock()
	name, cacheFresh := r.cache[ip], time.Now().Before(r.expires)
	r.mu.RUnlock()
	if cacheFresh {
		return ip, name
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pods, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return ip, ""
	}
	fresh := map[string]string{}
	for _, pod := range pods.Items {
		for _, podIP := range pod.Status.PodIPs {
			if podIP.IP != "" {
				fresh[podIP.IP] = pod.Namespace + "/" + pod.Name
			}
		}
		if pod.Status.PodIP != "" {
			fresh[pod.Status.PodIP] = pod.Namespace + "/" + pod.Name
		}
	}
	r.mu.Lock()
	r.cache = fresh
	r.expires = time.Now().Add(30 * time.Second)
	name = fresh[ip]
	r.mu.Unlock()
	return ip, name
}

func sourceIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return strings.Trim(remoteAddr, "[]")
}
