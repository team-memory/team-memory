package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"

	"github.com/team-memory/team-memory/internal/auth"
)

// Endpoint paths. The authorization server lives under /oauth/ on the same
// origin as the MCP endpoint, so its issuer is the deployment's origin.
const (
	pathAuthorize = "/oauth/authorize"
	pathToken     = "/oauth/token"
	pathJWKS      = "/oauth/jwks"
	pathRegister  = "/oauth/register"
	pathRevoke    = "/oauth/revoke"
	pathLogin     = "/oauth/login"
	pathCallback  = "/oauth/callback/"

	// ProtectedResourcePath is the RFC 9728 metadata URL for /mcp (path
	// insertion); the root form is served too.
	ProtectedResourcePath = "/.well-known/oauth-protected-resource/mcp"
)

// Options carries dependencies that are not configuration.
type Options struct {
	Identities *auth.Identities
	Logger     *slog.Logger
	// HTTPClient is used for upstream discovery and code exchange.
	HTTPClient *http.Client
	// MetadataClient fetches client metadata documents. Tests replace it;
	// the default refuses non-public addresses.
	MetadataClient *http.Client
}

// Server is the authorization server facade.
type Server struct {
	cfg             *Config
	keys            *Keyring
	store           *store
	clients         *clientRegistry
	provider        *op.Provider
	upstreams       map[string]*upstream
	upstreamOrigins []string
	identities      *auth.Identities
	verifier        *AccessTokens
	logins          *logins
	logger          *slog.Logger
	now             func() time.Time
}

// New loads secrets and builds the server. Errors name the missing or
// invalid setting and never include secret values.
func New(cfg *Config, opts Options) (*Server, error) {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	keys, err := LoadKeyring(cfg.SigningKeysDir, cfg.ActiveSigningKey)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg: cfg, keys: keys, upstreams: map[string]*upstream{}, identities: opts.Identities,
		logins: &logins{m: map[string]*pendingLogin{}}, logger: opts.Logger, now: time.Now,
	}
	s.clients = newClientRegistry(cfg.Registration, cfg.Issuer+pathLogin)
	if opts.MetadataClient != nil {
		s.clients.fetch = opts.MetadataClient
	}
	s.store = newStore(cfg, keys, s.clients)
	for _, p := range cfg.Providers {
		up := newUpstream(p, cfg.Issuer+pathCallback+p.ID, opts.HTTPClient)
		if _, err := up.clientSecret(); err != nil {
			return nil, err
		}
		s.upstreams[p.ID] = up
		if u, err := url.Parse(p.Issuer); err == nil {
			s.upstreamOrigins = append(s.upstreamOrigins, u.Scheme+"://"+u.Host)
		}
	}
	s.verifier = NewAccessTokens(cfg.Issuer, cfg.Resource(), keys, opts.Identities)

	opOpts := []op.Option{
		op.WithCustomAuthEndpoint(op.NewEndpoint(strings.TrimPrefix(pathAuthorize, "/"))),
		op.WithCustomTokenEndpoint(op.NewEndpoint(strings.TrimPrefix(pathToken, "/"))),
		op.WithCustomKeysEndpoint(op.NewEndpoint(strings.TrimPrefix(pathJWKS, "/"))),
		op.WithCustomRevocationEndpoint(op.NewEndpoint(strings.TrimPrefix(pathRevoke, "/"))),
		op.WithCustomUserinfoEndpoint(op.NewEndpoint("oauth/userinfo")),
		op.WithCustomIntrospectionEndpoint(op.NewEndpoint("oauth/introspect")),
		op.WithCustomEndSessionEndpoint(op.NewEndpoint("oauth/end_session")),
		op.WithCustomDeviceAuthorizationEndpoint(op.NewEndpoint("oauth/device_authorization")),
		op.WithLogger(opts.Logger),
	}
	if strings.HasPrefix(cfg.Issuer, "http://") {
		opOpts = append(opOpts, op.WithAllowInsecure())
	}
	s.provider, err = op.NewProvider(&op.Config{
		CryptoKey:             keys.derivedKey("authorization-code"),
		CodeMethodS256:        true,
		GrantTypeRefreshToken: true,
		SupportedScopes:       []string{ScopeMemory, oidc.ScopeOpenID, oidc.ScopeOfflineAccess},
	}, s.store, op.StaticIssuer(cfg.Issuer), opOpts...)
	if err != nil {
		return nil, fmt.Errorf("authorization server: %w", err)
	}
	return s, nil
}

// Start runs upstream discovery until every provider has succeeded once,
// retrying failures. Readiness reports providers that have not.
func (s *Server) Start(ctx context.Context) {
	for _, up := range s.upstreams {
		go func() {
			delay := time.Second
			for {
				err := up.discover(ctx)
				if err == nil {
					s.logger.Info("identity provider ready", "provider", up.cfg.ID)
					return
				}
				s.logger.Warn("identity provider not ready", "provider", up.cfg.ID, "err", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
				}
				delay = min(delay*2, time.Minute)
			}
		}()
	}
}

// Ready reports whether the facade can sign users in. It rereads the
// secret files, so a deleted or emptied Secret makes the server not ready.
func (s *Server) Ready() error {
	var errs []error
	for _, up := range s.upstreams {
		if err := up.status(); err != nil {
			errs = append(errs, err)
		}
		if _, err := up.clientSecret(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Verifier returns the access-token authenticator.
func (s *Server) Verifier() *AccessTokens { return s.verifier }

// ResourceMetadataURL is advertised in WWW-Authenticate challenges.
func (s *Server) ResourceMetadataURL() string { return s.cfg.Issuer + ProtectedResourcePath }

// Register mounts the facade's routes on mux.
func (s *Server) Register(mux *http.ServeMux) {
	prm := http.HandlerFunc(s.protectedResource)
	mux.Handle("GET /.well-known/oauth-protected-resource", prm)
	mux.Handle("GET "+ProtectedResourcePath, prm)
	asm := http.HandlerFunc(s.authServerMetadata)
	mux.Handle("GET /.well-known/oauth-authorization-server", asm)
	mux.Handle("GET /.well-known/openid-configuration", asm)
	if s.cfg.Registration.Dynamic {
		mux.HandleFunc(pathRegister, s.clients.register)
	}
	mux.HandleFunc("GET "+pathLogin, s.consent)
	mux.HandleFunc("POST "+pathLogin, s.approve)
	mux.HandleFunc("GET "+pathCallback+"{provider}", s.callback)
	mux.Handle(pathAuthorize, s.withResource(s.provider))
	mux.Handle(pathToken, s.withResource(s.provider))
	// Remaining op endpoints: authorize callback, JWKS, revocation,
	// userinfo. Introspection, end_session and device authorization are
	// not advertised, and the storage refuses them.
	mux.Handle("/oauth/", s.provider)
}

// withResource validates the RFC 8707 resource parameter on authorization
// and token requests. The only acceptable resource is the canonical MCP
// endpoint URI; any other value is refused with invalid_target.
func (s *Server) withResource(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		res := r.Form["resource"]
		if len(res) > 1 {
			invalidTarget(w, "only one resource may be requested")
			return
		}
		if len(res) == 1 {
			if !sameResource(res[0], s.cfg.Resource()) {
				invalidTarget(w, "unknown resource; the only resource is "+s.cfg.Resource())
				return
			}
			r = r.WithContext(withResource(r.Context(), s.cfg.Resource()))
		}
		next.ServeHTTP(w, r)
	})
}

func invalidTarget(w http.ResponseWriter, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_target", "error_description": desc})
}

// sameResource compares URIs after the normalization RFC 3986 section 6.2.2
// allows (case of scheme and host, default port) and ignoring one trailing
// slash, which MCP clients are inconsistent about.
func sameResource(a, b string) bool {
	norm := func(s string) string {
		u, err := url.Parse(s)
		if err != nil || u.Fragment != "" || u.RawQuery != "" {
			return ""
		}
		host := strings.ToLower(u.Host)
		scheme := strings.ToLower(u.Scheme)
		host = strings.TrimSuffix(host, map[string]string{"https": ":443", "http": ":80"}[scheme])
		return scheme + "://" + host + strings.TrimSuffix(u.EscapedPath(), "/")
	}
	na := norm(a)
	return na != "" && na == norm(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) protectedResource(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"resource":                 s.cfg.Resource(),
		"authorization_servers":    []string{s.cfg.Issuer},
		"scopes_supported":         []string{ScopeMemory},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "Team Memory",
	})
}

// authServerMetadata is RFC 8414 metadata, also served as the OpenID
// configuration so that clients trying either URL see the same document.
func (s *Server) authServerMetadata(w http.ResponseWriter, _ *http.Request) {
	iss := s.cfg.Issuer
	m := map[string]any{
		"issuer":                                     iss,
		"authorization_endpoint":                     iss + pathAuthorize,
		"token_endpoint":                             iss + pathToken,
		"jwks_uri":                                   iss + pathJWKS,
		"revocation_endpoint":                        iss + pathRevoke,
		"scopes_supported":                           []string{ScopeMemory, oidc.ScopeOfflineAccess},
		"response_types_supported":                   []string{"code"},
		"response_modes_supported":                   []string{"query"},
		"grant_types_supported":                      []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":      []string{"none"},
		"revocation_endpoint_auth_methods_supported": []string{"none"},
		"code_challenge_methods_supported":           []string{"S256"},
		"client_id_metadata_document_supported":      s.cfg.Registration.ClientIDMetadataDocuments,
		"subject_types_supported":                    []string{"public"},
		"id_token_signing_alg_values_supported":      s.keys.Algorithms(),
	}
	if s.cfg.Registration.Dynamic {
		m["registration_endpoint"] = iss + pathRegister
	}
	writeJSON(w, m)
}
