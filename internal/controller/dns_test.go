package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	platformv1alpha1 "github.com/einyx/kubo/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// cfStub emulates the Cloudflare API surface the reconciler uses.
type cfStub struct {
	mu      sync.Mutex
	records map[string]cfRecord
	created int
	updated int
	deleted int
}

type cfRecord struct {
	ID      string
	Content string
	Comment string
}

func newCFStub() *cfStub { return &cfStub{records: map[string]cfRecord{}} }

func (s *cfStub) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/zones":
			writeJSON(w, map[string]interface{}{"success": true,
				"result": []map[string]interface{}{{"id": "zone-1", "name": "kubify.foundation"}}})
		case strings.Contains(r.URL.Path, "/dns_records") && r.Method == http.MethodGet:
			out := []map[string]interface{}{}
			name := r.URL.Query().Get("name")
			s.mu.Lock()
			for host, rec := range s.records {
				if name != "" && host != name {
					continue
				}
				out = append(out, map[string]interface{}{
					"id": rec.ID, "type": "CNAME", "name": host,
					"content": rec.Content, "proxied": true, "comment": rec.Comment,
				})
			}
			s.mu.Unlock()
			writeJSON(w, map[string]interface{}{"success": true, "result": out})
		case strings.Contains(r.URL.Path, "/dns_records") && r.Method == http.MethodPost:
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			s.created++
			comment, _ := body["comment"].(string)
			s.records[body["name"].(string)] = cfRecord{
				ID:      body["name"].(string) + "-id",
				Content: body["content"].(string),
				Comment: comment,
			}
			s.mu.Unlock()
			writeJSON(w, map[string]interface{}{"success": true, "result": map[string]interface{}{"id": "new"}})
		case strings.Contains(r.URL.Path, "/dns_records/") && (r.Method == http.MethodPut || r.Method == http.MethodPatch):
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			s.mu.Lock()
			s.updated++
			host := body["name"].(string)
			s.records[host] = cfRecord{ID: host + "-id", Content: body["content"].(string), Comment: "managed-by:kubo stack=ns/name"}
			s.mu.Unlock()
			writeJSON(w, map[string]interface{}{"success": true, "result": map[string]interface{}{"id": host + "-id"}})
		case strings.Contains(r.URL.Path, "/dns_records/") && r.Method == http.MethodDelete:
			parts := strings.Split(r.URL.Path, "/")
			id := parts[len(parts)-1]
			s.mu.Lock()
			s.deleted++
			for host, rec := range s.records {
				if rec.ID == id {
					delete(s.records, host)
				}
			}
			s.mu.Unlock()
			writeJSON(w, map[string]interface{}{"success": true})
		default:
			t.Logf("unexpected CF call: %s %s", r.Method, r.URL.Path)
			writeJSON(w, map[string]interface{}{"success": true})
		}
	})
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	json.NewEncoder(w).Encode(v)
}

func dnsTestStack(host string) *platformv1alpha1.Stack {
	return &platformv1alpha1.Stack{
		ObjectMeta: metav1.ObjectMeta{Name: "foundation", Namespace: "tenant-x"},
		Spec: platformv1alpha1.StackSpec{
			VirtualService: &platformv1alpha1.StackVirtualService{
				Host:            host,
				Gateway:         "foundation/foundation-gateway",
				AdditionalHosts: []string{host + "-legacy"},
				HTTP:            []apiextensionsv1.JSON{{}},
			},
		},
	}
}

// Full lifecycle: create on first pass, no-op on identical second pass,
// heal on drift (PUT), and delete only kubo-tagged records.
func TestDNSManageHealAndDelete(t *testing.T) {
	stub := newCFStub()
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	restore := setCFBase(srv.URL)
	defer restore()

	sch := runtime.NewScheme()
	clientgoscheme.AddToScheme(sch)
	corev1.AddToScheme(sch)
	platformv1alpha1.AddToScheme(sch)
	c := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: cfAPISecret, Namespace: sourceNamespace},
			Data: map[string][]byte{cfAPITokenKey: []byte("tok")}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cloudflaredConfigName, Namespace: sourceNamespace},
			Data: map[string]string{"config.yaml": "tunnel: tunnel-1\ningress:\n  - service: http_status:404"}},
	).Build()
	r := &StackReconciler{Client: c, Scheme: sch}
	stack := dnsTestStack("foundation-x.kubify.foundation")
	ctx := context.Background()

	// 1. Create both hosts.
	if err := r.ensureTenantDNS(ctx, stack); err != nil {
		t.Fatal(err)
	}
	if stub.created != 2 {
		t.Fatalf("created: %d", stub.created)
	}

	// 2. Second identical pass: no churn.
	createdBefore, updatedBefore := stub.created, stub.updated
	if err := r.ensureTenantDNS(ctx, stack); err != nil {
		t.Fatal(err)
	}
	if stub.created != createdBefore || stub.updated != updatedBefore {
		t.Fatalf("no-op pass mutated records: created %d->%d updated %d->%d", createdBefore, stub.created, updatedBefore, stub.updated)
	}

	// 3. Simulate drift: content flipped out-of-band → next pass heals (PUT).
	stub.mu.Lock()
	rec := stub.records["foundation-x.kubify.foundation"]
	rec.Content = "wrong-target.cfargotunnel.com"
	stub.records["foundation-x.kubify.foundation"] = rec
	stub.mu.Unlock()
	if err := r.ensureTenantDNS(ctx, stack); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	rec2 := stub.records["foundation-x.kubify.foundation"]
	healed := rec2.Content == "tunnel-1.cfargotunnel.com"
	stub.mu.Unlock()
	if !healed {
		t.Fatal("drift not healed via PUT")
	}

	// 4. Delete: both tagged records removed.
	if err := r.deleteTenantDNS(ctx, stack); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	remaining := len(stub.records)
	stub.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("tagged records not deleted: %d remain", remaining)
	}
}

// A record without the kubo tag (human-managed) must never be deleted.
func TestDNSDeleteSkipsUntagged(t *testing.T) {
	stub := newCFStub()
	stub.records["foundation-x.kubify.foundation"] = cfRecord{ID: "human", Comment: "created by hand"}
	srv := httptest.NewServer(stub.handler(t))
	defer srv.Close()
	restore := setCFBase(srv.URL)
	defer restore()

	sch := runtime.NewScheme()
	clientgoscheme.AddToScheme(sch)
	corev1.AddToScheme(sch)
	platformv1alpha1.AddToScheme(sch)
	c := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &StackReconciler{Client: c, Scheme: sch}
	// deleteTenantDNS with a nil virtualService stack: nothing to do, no calls.
	stack := &platformv1alpha1.Stack{ObjectMeta: metav1.ObjectMeta{Name: "f", Namespace: "n"}}
	if err := r.deleteTenantDNS(context.Background(), stack); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.deleted != 0 {
		t.Fatal("deleted without host match")
	}
}

// Without the token secret the reconciler must skip DNS management entirely
// (disabled), not error.
func TestDNSDisabledWithoutToken(t *testing.T) {
	sch := runtime.NewScheme()
	clientgoscheme.AddToScheme(sch)
	corev1.AddToScheme(sch)
	platformv1alpha1.AddToScheme(sch)
	c := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &StackReconciler{Client: c, Scheme: sch}
	if err := r.ensureTenantDNS(context.Background(), dnsTestStack("foundation-x.kubify.foundation")); err != nil {
		t.Fatalf("disabled DNS should be a silent no-op, got: %v", err)
	}
}

func setCFBase(u string) (restore func()) {
	old := cfAPIBase
	cfAPIBase = u
	return func() { cfAPIBase = old }
}
