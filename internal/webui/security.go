package webui

import (
	"net/http"
	"strings"
)

// browserSecurity covers public pages and authentication failures as well as
// authenticated handlers. ListenAndServe applies trusted proxy headers before
// this handler, so the CSRF origin check sees the public Host behind a proxy.
func browserSecurity(next http.Handler) http.Handler {
	protected := http.NewCrossOriginProtection().Handler(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		// The existing templates use inline scripts, event handlers and styles.
		// Keep those working while restricting resources, framing and form targets.
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		protected.ServeHTTP(w, r)
	})
}
