package portal

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestFrontendAssets(t *testing.T) {
	p := newFake(t)
	handler := p.Mux()
	html := p.GetIndexHTML()
	if !strings.Contains(html, `class="meshx-theme"`) || !strings.Contains(html, `/assets/meshx-design-system.css`) {
		t.Fatal("portal is not wired to the MeshX design system")
	}
	if strings.Contains(html, "fonts.googleapis.com") {
		t.Fatal("portal must use the design system's self-hosted fonts")
	}
	refs := regexp.MustCompile(`(?:src|href)="(/assets/[^"]+)"`).FindAllStringSubmatch(p.GetIndexHTML(), -1)
	if len(refs) == 0 {
		t.Fatal("page has no asset references")
	}
	for _, font := range []string{
		"/assets/fonts/stack-sans-headline-latin-wght-normal.woff2",
		"/assets/fonts/stack-sans-text-latin-wght-normal.woff2",
		"/assets/fonts/jetbrains-mono-latin-wght-normal.woff2",
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://localhost"+font, nil))
		if w.Code != http.StatusOK || w.Body.Len() == 0 {
			t.Fatalf("font %s response: %d", font, w.Code)
		}
	}
	for _, ref := range refs {
		t.Run(ref[1], func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://localhost"+ref[1], nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != http.StatusOK || w.Body.Len() == 0 {
				t.Fatalf("asset response: %d %s", w.Code, w.Body.String())
			}
			contentType := w.Header().Get("Content-Type")
			if strings.HasSuffix(ref[1], ".css") && !strings.Contains(contentType, "text/css") {
				t.Fatalf("CSS content type: %s", contentType)
			}
			if strings.HasSuffix(ref[1], ".js") && !strings.Contains(contentType, "javascript") {
				t.Fatalf("JavaScript content type: %s", contentType)
			}
		})
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://localhost/assets/missing.js", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing asset status: %d", w.Code)
	}
}
