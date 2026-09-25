package portal

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRemoteMuxHostPolicy(t *testing.T) {
	p := newFake(t)
	for _, tc := range []struct {
		name    string
		handler http.Handler
		want    int
	}{
		{"local", p.Mux(), http.StatusForbidden},
		{"remote", p.RemoteMux(), http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://kubo-portal.kubo-system.svc:9090/api/stacks", nil)
			w := httptest.NewRecorder()
			tc.handler.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}
