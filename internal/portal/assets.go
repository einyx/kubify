package portal

import (
	"embed"
	"net/http"
)

// Assets ship with the portal binary; no frontend build or runtime is required.
//
//go:embed assets/*.css assets/*.js
var assetFS embed.FS

func serveAsset(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	http.FileServerFS(assetFS).ServeHTTP(w, r)
}
