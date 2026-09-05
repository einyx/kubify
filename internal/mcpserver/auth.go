package mcpserver

import (
	"net/http"
	"strings"
)

// wrapTokenAuth guards the SSE transport with a static bearer token when one
// is configured. Requests without a matching Authorization header are
// rejected before a session is created. The stdio transport is local and
// needs no token.
func wrapTokenAuth(next http.Handler, token string) http.Handler {
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			// Also accept the token as a query param: SSE clients that
			// cannot set headers on the POST endpoint pass it here.
			auth = "Bearer " + r.URL.Query().Get("token")
		}
		if !strings.EqualFold(auth, "Bearer "+token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
