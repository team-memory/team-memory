package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicOnboarding(t *testing.T) {
	for _, tc := range []struct {
		name, publicURL, portalPath string
		sso                         bool
	}{
		{"tokens", "https://memory.example.com", "/portal/", false},
		{"sso", "https://memory.example.com", "/knowledge/", true},
		{"disabled", "", "", false},
		{"root portal", "https://memory.example.com", "/", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			if tc.portalPath == "/" {
				mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
			}
			RegisterPublic(mux, tc.publicURL, tc.portalPath, tc.sso)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", "/welcome", nil))
			body := w.Body.String()
			if w.Code != 200 || !strings.Contains(body, "codex mcp add") || !strings.Contains(body, "claude mcp add") {
				t.Fatalf("onboarding missing: %d %s", w.Code, body)
			}
			if strings.Contains(body, "--bearer-token-env-var") == tc.sso {
				t.Error("wrong authentication instructions")
			}
			if tc.portalPath != "" && !strings.Contains(body, `href="`+tc.portalPath+`"`) {
				t.Error("portal path missing")
			}
			if tc.portalPath == "" && strings.Contains(body, "Open portal") {
				t.Error("disabled portal linked")
			}
			if tc.publicURL != "" && !strings.Contains(body, tc.publicURL+"/mcp") {
				t.Error("public endpoint missing")
			}
			w = httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
			want := 200
			if tc.portalPath == "/" {
				want = 204
			}
			if w.Code != want {
				t.Fatalf("root: got %d want %d", w.Code, want)
			}
			w = httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", "/unknown", nil))
			if w.Code != 404 {
				t.Error("unknown path must remain 404")
			}
			w = httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", "/welcome/brand.css", nil))
			if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "text/css") {
				t.Error("stylesheet unavailable")
			}
		})
	}
}
