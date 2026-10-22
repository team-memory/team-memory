package telemetry

import "testing"

func TestRoute(t *testing.T) {
	for _, c := range []struct{ path, base, want string }{
		{"/mcp", "/portal", "/mcp"},
		{"/mcp/extra", "/portal", "other"},
		{"/oauth/callback/keycloak", "/portal", "/oauth"},
		{"/.well-known/oauth-protected-resource", "/portal", "/.well-known"},
		{"/portal", "/portal", "/portal"},
		{"/portal/m/123", "/portal", "/portal"},
		{"/portalx", "/portal", "other"},
		{"/anything", "/", "/portal"},
		{"/anything", "", "other"},
		{"/healthz", "", "/healthz"},
		{"/readyz", "/", "/readyz"},
	} {
		if got := Route(c.path, c.base); got != c.want {
			t.Errorf("Route(%q, %q) = %q, want %q", c.path, c.base, got, c.want)
		}
	}
}
