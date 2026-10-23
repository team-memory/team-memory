package portal

import (
	"html/template"
	"net/http"
	"net/url"
)

// RegisterPublic exposes onboarding without requiring a portal session. An
// explicitly root-mounted portal keeps its home; onboarding lives at /welcome.
func RegisterPublic(mux *http.ServeMux, publicURL, portalPath string, sso bool) {
	endpoint := "https://YOUR_MEMORY_HOST/mcp"
	if u, err := url.Parse(publicURL); err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http") {
		endpoint = u.Scheme + "://" + u.Host + "/mcp"
	}
	t := template.Must(template.ParseFS(assets, "public/home.html"))
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_ = t.Execute(w, struct {
			Endpoint, Portal string
			SSO              bool
		}{endpoint, portalPath, sso})
	}
	mux.HandleFunc("GET /welcome", handler)
	if portalPath != "/" {
		mux.HandleFunc("GET /{$}", handler)
	}
	mux.HandleFunc("GET /welcome/brand.css", func(w http.ResponseWriter, r *http.Request) {
		b, _ := assets.ReadFile("static/portal.css")
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(b)
	})
}
