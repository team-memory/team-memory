package integration

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/team-memory/team-memory/internal/auth"
	"github.com/team-memory/team-memory/internal/search"
	"github.com/team-memory/team-memory/internal/server"
)

// TestSSOMisconfigurationFailsClosed: with SSO configured but broken, the
// server is not ready, /readyz says why, and nothing is served, not even to
// a valid personal access token. There is no fallback mode.
func TestSSOMisconfigurationFailsClosed(t *testing.T) {
	cases := map[string]struct {
		edit func(cfg map[string]any, secretPath string)
		want string
	}{
		"missing secret file": {func(cfg map[string]any, _ string) {
			cfg["providers"].([]map[string]any)[0]["client_secret_file"] = "/nonexistent/client-secret"
		}, `provider "corp" client secret`},
		"empty secret": {func(_ map[string]any, secretPath string) {
			_ = os.WriteFile(secretPath, []byte("\n"), 0o600)
		}, "is empty"},
		"missing signing keys": {func(cfg map[string]any, _ string) {
			cfg["signing_keys_dir"] = "/nonexistent/keys"
		}, "signing keys"},
		"inline secret": {func(cfg map[string]any, _ string) {
			cfg["providers"].([]map[string]any)[0]["client_secret"] = "inline"
		}, "unknown field"},
		"unreachable provider": {func(cfg map[string]any, _ string) {
			cfg["providers"].([]map[string]any)[0]["issuer"] = "http://127.0.0.1:1/realms/none"
		}, "discovery"},
		"plain http issuer": {func(cfg map[string]any, _ string) {
			cfg["allow_insecure_http"] = false
		}, "https"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := newSSOEnv(t, c.edit)
			tok, err := auth.NewPATs(env.tr.db.Admin).Issue(context.Background(), env.tr.owner.ID, "ci", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond) // let discovery fail at least once

			resp, err := http.Get(env.url + "/readyz")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), c.want) {
				t.Fatalf("readyz: %d %q, want 503 mentioning %q", resp.StatusCode, body, c.want)
			}
			if strings.Contains(string(body), env.idp.clientSecret) {
				t.Fatal("readyz leaked the client secret")
			}
			for _, bearer := range []string{"", tok.Secret} {
				req, _ := http.NewRequest(http.MethodPost, env.url+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
				req.Header.Set("Content-Type", "application/json")
				if bearer != "" {
					req.Header.Set("Authorization", "Bearer "+bearer)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusServiceUnavailable {
					t.Fatalf("/mcp served while SSO is broken (bearer=%t): %d", bearer != "", resp.StatusCode)
				}
			}
			resp, err = http.Get(env.url + "/oauth/authorize?client_id=x")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("/oauth served while SSO is broken: %d", resp.StatusCode)
			}
			for _, path := range []string{"/portal/login", "/portal/", "/portal/s/acme"} {
				resp, err = http.Get(env.url + path)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusServiceUnavailable {
					t.Fatalf("%s served while SSO is broken: %d", path, resp.StatusCode)
				}
			}
			resp, err = http.Get(env.url + "/.well-known/oauth-protected-resource/mcp")
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("metadata served while SSO is broken: %d", resp.StatusCode)
			}
		})
	}
}

// TestSSOSecretRemovedAtRuntime: deleting the mounted secret after startup
// makes the server not ready within the cache window.
func TestSSOSecretRemovedAtRuntime(t *testing.T) {
	env := newSSOEnv(t, nil)
	env.waitReady(t)
	if err := os.Remove(env.secretPath); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for env.handler.Ready(context.Background()) == nil {
		if time.Now().After(deadline) {
			t.Fatal("still ready after the secret was removed")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestSoloBootstrapNoIdP is the solo path end to end: init, then an MCP
// client connects to the assembled server with the init token. No SSO
// configuration exists, and OAuth endpoints are absent.
func TestSoloBootstrapNoIdP(t *testing.T) {
	ctx := context.Background()
	pool := newDB(t)
	res, err := auth.Init(ctx, pool, auth.InitOptions{OrgSlug: "solo", OrgName: "Solo", ProjectSlug: "notes", OwnerName: "Me"})
	if err != nil {
		t.Fatal(err)
	}
	tr := &tree{pool: pool}
	srv := httptest.NewServer(server.New(server.Options{Pool: pool, Search: search.New(pool, search.Config{})}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz without SSO: %v %d", err, resp.StatusCode)
	}
	resp.Body.Close()
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/oauth/authorize"} {
		resp, _ := http.Get(srv.URL + p)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s exists in solo mode: %d", p, resp.StatusCode)
		}
	}
	m := &mcpEnv{url: srv.URL + "/mcp", tr: tr}
	if _, err := m.connect(t, "", ""); err == nil {
		t.Fatal("connected without a token")
	}
	cs, err := m.connect(t, res.Token.Secret, "")
	if err != nil {
		t.Fatal(err)
	}
	out, isErr := call(t, cs, "memory_create", map[string]any{"scope": "solo/notes", "kind": "fact", "title": "Solo works", "content": "No IdP needed."})
	if isErr {
		t.Fatalf("memory_create: %v", out)
	}
}
