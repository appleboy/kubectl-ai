// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package mcpauth turns the kubectl-ai MCP server (streamable-http transport)
// into an OAuth 2.1 Resource Server. It verifies incoming Bearer access tokens
// locally against an Authorization Server's JWKS and publishes a Protected
// Resource Metadata document (RFC 9728) so MCP clients can discover the AS.
//
// Authentication is purely opt-in: a zero-value Config (empty issuer) reports
// Enabled() == false and the caller keeps its previous, unauthenticated
// behavior.
package mcpauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
	"k8s.io/klog/v2"
)

// PRMPath is the well-known path for the Protected Resource Metadata document
// (RFC 9728). It is served without authentication.
const PRMPath = "/.well-known/oauth-protected-resource"

// clockLeeway tolerates small clock skew between the Authorization Server and
// this Resource Server when validating time-based claims (exp/nbf/iat).
const clockLeeway = 30 * time.Second

// httpTimeout bounds each JWKS / discovery HTTP request so a slow or
// black-holed Authorization Server cannot block kubectl-ai startup.
const httpTimeout = 10 * time.Second

// retryInterval is how often we re-attempt to load signing keys after an
// initial failure to reach the Authorization Server.
const retryInterval = 30 * time.Second

// Config describes how the MCP server authenticates incoming requests.
type Config struct {
	// Issuer is the AuthGate base URL, expected as the JWT `iss` claim.
	// An empty Issuer disables authentication entirely.
	Issuer string
	// Audience is this Resource Server's identifier, expected as the JWT `aud`
	// claim (for example "https://kubectl-ai.corp/mcp").
	Audience string
	// JWKSURL optionally overrides the JWKS URL. When empty it is discovered
	// from <Issuer>/.well-known/openid-configuration.
	JWKSURL string
}

// Enabled reports whether authentication is configured. When false the caller
// should keep its previous, unauthenticated behavior.
func (c Config) Enabled() bool { return c.Issuer != "" }

// validate checks configuration that can be verified without any network
// access, so obvious mistakes (typos) fail fast at startup.
func (c Config) validate() error {
	if c.Issuer == "" {
		return fmt.Errorf("issuer must not be empty when authentication is enabled")
	}
	if !isAbsoluteURL(c.Issuer) {
		return fmt.Errorf("issuer %q is not a valid absolute URL", c.Issuer)
	}
	if c.Audience == "" {
		return fmt.Errorf("audience is required when issuer is set")
	}
	if c.JWKSURL != "" && !isAbsoluteURL(c.JWKSURL) {
		return fmt.Errorf("jwks-url %q is not a valid absolute URL", c.JWKSURL)
	}
	return nil
}

// isAbsoluteURL reports whether s parses as an absolute URL with a host.
func isAbsoluteURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.IsAbs() && u.Host != ""
}

// Verifier validates Bearer tokens against the Authorization Server's JWKS.
//
// Startup strategy (never fail-open):
//   - Configuration errors that need no network (bad issuer URL, missing
//     audience) fail fast in NewVerifier.
//   - A temporarily unreachable Authorization Server does NOT crash the
//     process: NewVerifier logs a warning and keeps retrying in the background.
//   - While signing keys are not yet loaded, Middleware returns 503 for every
//     request (fail-closed) and never lets unverified traffic through.
type Verifier struct {
	cfg Config
	// parser holds the immutable claim-validation rules (iss/aud/exp/alg/leeway).
	// It is built once in NewVerifier and reused for every request so the option
	// closures are not re-allocated on the hot path.
	parser *jwt.Parser

	mu sync.RWMutex
	kf keyfunc.Keyfunc // nil until signing keys are loaded
}

// NewVerifier validates the config, then attempts to load signing keys.
//
// It returns an error only for configuration problems (fail-fast). If the
// Authorization Server is merely unreachable, it returns a usable Verifier that
// reports "not ready" (Middleware -> 503) and loads keys in the background.
func NewVerifier(ctx context.Context, cfg Config) (*Verifier, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	v := &Verifier{
		cfg: cfg,
		parser: jwt.NewParser(
			jwt.WithIssuer(cfg.Issuer),
			jwt.WithAudience(cfg.Audience),
			jwt.WithExpirationRequired(),
			jwt.WithValidMethods([]string{"RS256", "ES256"}),
			jwt.WithLeeway(clockLeeway),
		),
	}

	kf, err := v.buildKeyfunc(ctx)
	if err != nil {
		klog.Warningf("MCP auth: could not load signing keys from authorization server %q (%v); "+
			"starting anyway. /mcp will return 503 until keys are retrieved in the background.", cfg.Issuer, err)
		go v.retryLoad(ctx)
		return v, nil
	}

	v.setKeyfunc(kf)
	klog.V(1).Infof("MCP auth enabled: issuer=%q audience=%q jwks_url=%q", cfg.Issuer, cfg.Audience, cfg.JWKSURL)
	return v, nil
}

// buildKeyfunc resolves the JWKS URL (via discovery unless overridden) and
// builds a keyfunc. NoErrorReturnFirstHTTPReq is set to false so that a network
// failure surfaces here (and we retry) instead of silently producing a keyfunc
// with no keys.
func (v *Verifier) buildKeyfunc(ctx context.Context) (keyfunc.Keyfunc, error) {
	jwksURL := v.cfg.JWKSURL
	if jwksURL == "" {
		discovered, err := discoverJWKSURL(ctx, v.cfg.Issuer)
		if err != nil {
			return nil, err
		}
		jwksURL = discovered
	}

	failOnFirstError := false
	kf, err := keyfunc.NewDefaultOverrideCtx(ctx, []string{jwksURL}, keyfunc.Override{
		NoErrorReturnFirstHTTPReq: &failOnFirstError,
		HTTPTimeout:               httpTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("loading JWKS from %q: %w", jwksURL, err)
	}
	return kf, nil
}

// retryLoad keeps trying to load signing keys until it succeeds or ctx is done.
// Key rotation after the first success is handled automatically by keyfunc.
func (v *Verifier) retryLoad(ctx context.Context) {
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		kf, err := v.buildKeyfunc(ctx)
		if err != nil {
			klog.Warningf("MCP auth: still unable to load signing keys from %q: %v", v.cfg.Issuer, err)
			continue
		}
		v.setKeyfunc(kf)
		klog.Infof("MCP auth: loaded signing keys from %q; /mcp authentication is now active", v.cfg.Issuer)
		return
	}
}

func (v *Verifier) setKeyfunc(kf keyfunc.Keyfunc) {
	v.mu.Lock()
	v.kf = kf
	v.mu.Unlock()
}

func (v *Verifier) keyfunc() (keyfunc.Keyfunc, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.kf, v.kf != nil
}

// Middleware wraps next so that only requests carrying a valid access token are
// passed through. Requests are rejected with 503 while keys are not yet loaded
// and 401 when the token is missing or invalid.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kf, ready := v.keyfunc()
		if !ready {
			// Fail-closed: keys not yet available, do not attempt to verify and
			// do not let the request through.
			klog.V(2).Info("MCP auth: signing keys not ready, returning 503")
			w.Header().Set("Retry-After", "5")
			http.Error(w, "authorization server signing keys are not yet available", http.StatusServiceUnavailable)
			return
		}

		const bearerPrefix = "Bearer "
		authz := r.Header.Get("Authorization")
		if len(authz) < len(bearerPrefix) || !strings.EqualFold(authz[:len(bearerPrefix)], bearerPrefix) {
			v.reject(w, r, "missing or malformed Authorization header")
			return
		}
		tokenStr := strings.TrimSpace(authz[len(bearerPrefix):])
		if tokenStr == "" {
			v.reject(w, r, "empty bearer token")
			return
		}

		token, err := v.parser.Parse(tokenStr, kf.Keyfunc)
		if err != nil || !token.Valid {
			v.reject(w, r, fmt.Sprintf("token validation failed: %v", err))
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			v.reject(w, r, "unexpected claims type")
			return
		}
		// The `type` claim check is security-critical: refresh tokens are signed
		// with the same key as access tokens, so without this check a refresh
		// token could be presented in place of an access token.
		if t, _ := claims["type"].(string); t != "access" {
			v.reject(w, r, "token type is not \"access\"")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// reject writes a 401 with a WWW-Authenticate header pointing MCP clients at
// the Protected Resource Metadata document.
func (v *Verifier) reject(w http.ResponseWriter, r *http.Request, reason string) {
	klog.V(2).Infof("MCP auth: rejecting request: %s", reason)
	w.Header().Set("WWW-Authenticate", wwwAuthenticate(r))
	http.Error(w, "invalid token", http.StatusUnauthorized)
}

// wwwAuthenticate builds the RFC 9728 WWW-Authenticate challenge advertising the
// Protected Resource Metadata URL. The scheme prefers X-Forwarded-Proto so the
// URL is correct behind a reverse proxy.
func wwwAuthenticate(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	prm := fmt.Sprintf("%s://%s%s", scheme, r.Host, PRMPath)
	return fmt.Sprintf("Bearer resource_metadata=%q, error=%q", prm, "invalid_token")
}

// PRMHandler serves the Protected Resource Metadata document (RFC 9728). It
// requires no authentication and carries permissive CORS headers so that
// browser-based MCP clients can fetch it cross-origin.
func (c Config) PRMHandler() http.HandlerFunc {
	body, _ := json.Marshal(map[string]any{
		"resource":                 c.Audience,
		"authorization_servers":    []string{c.Issuer},
		"bearer_methods_supported": []string{"header"},
	})
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// discoverJWKSURL fetches <issuer>/.well-known/openid-configuration and returns
// its jwks_uri. The request is bounded by httpTimeout so it cannot block
// startup indefinitely.
func discoverJWKSURL(ctx context.Context, issuer string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	metaURL := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return "", fmt.Errorf("building discovery request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching %q: %w", metaURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery %q returned status %d", metaURL, resp.StatusCode)
	}

	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", fmt.Errorf("decoding discovery document from %q: %w", metaURL, err)
	}
	if doc.JWKSURI == "" {
		return "", fmt.Errorf("discovery document %q has no jwks_uri", metaURL)
	}
	if doc.Issuer != "" && doc.Issuer != issuer {
		klog.Warningf("MCP auth: discovery issuer %q does not match configured issuer %q", doc.Issuer, issuer)
	}
	klog.V(1).Infof("MCP auth: discovered jwks_uri %q from %q", doc.JWKSURI, metaURL)
	return doc.JWKSURI, nil
}
