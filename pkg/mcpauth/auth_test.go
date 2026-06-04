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

package mcpauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testKID = "test-key-1"

// fixture bundles a self-signed RSA key, an httptest server that serves the
// matching JWKS (and an OIDC discovery document), and the resulting auth Config.
type fixture struct {
	key      *rsa.PrivateKey
	server   *httptest.Server
	issuer   string
	audience string
	jwksURL  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}

	f := &fixture{key: key, audience: "https://kubectl-ai.test/mcp"}

	jwksBytes := jwksJSON(t, testKID, &key.PublicKey)

	mux := http.NewServeMux()
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwksBytes)
	})
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":   f.issuer,
			"jwks_uri": f.jwksURL,
		})
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	f.issuer = f.server.URL
	f.jwksURL = f.server.URL + "/jwks"
	return f
}

// jwksJSON builds a minimal JWKS document containing the RSA public key.
func jwksJSON(t *testing.T, kid string, pub *rsa.PublicKey) []byte {
	t.Helper()
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	set := map[string]any{
		"keys": []map[string]any{
			{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid, "n": n, "e": e},
		},
	}
	b, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshaling JWKS: %v", err)
	}
	return b
}

func (f *fixture) validClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss":  f.issuer,
		"aud":  f.audience,
		"type": "access",
		"iat":  jwt.NewNumericDate(time.Now()),
		"exp":  jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
}

func (f *fixture) sign(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = testKID
	signed, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	return signed
}

// readyVerifier returns a Verifier with signing keys loaded, using the JWKS URL
// override so no discovery round trip is needed.
func (f *fixture) readyVerifier(t *testing.T) *Verifier {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	v, err := NewVerifier(ctx, Config{Issuer: f.issuer, Audience: f.audience, JWKSURL: f.jwksURL})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if _, ready := v.keyfunc(); !ready {
		t.Fatalf("verifier is not ready after NewVerifier with reachable JWKS")
	}
	return v
}

// runMiddleware sends req through v.Middleware and reports the response plus
// whether the wrapped (protected) handler was reached.
func runMiddleware(v *Verifier, req *http.Request) (*httptest.ResponseRecorder, bool) {
	nextCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("tool-output"))
	})
	rec := httptest.NewRecorder()
	v.Middleware(next).ServeHTTP(rec, req)
	return rec, nextCalled
}

func bearer(token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// Test 1: a well-formed access token is accepted and the request is forwarded.
func TestMiddleware_HappyPath(t *testing.T) {
	f := newFixture(t)
	v := f.readyVerifier(t)

	rec, nextCalled := runMiddleware(v, bearer(f.sign(t, f.validClaims())))

	if !nextCalled {
		t.Errorf("expected protected handler to be called")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

// Test 2: missing or malformed credentials yield 401 with a WWW-Authenticate
// header that points clients at the Protected Resource Metadata document.
func TestMiddleware_MissingOrMalformedToken(t *testing.T) {
	f := newFixture(t)
	v := f.readyVerifier(t)

	cases := map[string]*http.Request{
		"no header":   httptest.NewRequest(http.MethodPost, "/mcp", nil),
		"not bearer":  withHeader(httptest.NewRequest(http.MethodPost, "/mcp", nil), "Authorization", "Basic abc123"),
		"empty token": bearer(""),
	}

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			rec, nextCalled := runMiddleware(v, req)
			if nextCalled {
				t.Errorf("protected handler must not be called")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("expected status 401, got %d", rec.Code)
			}
			wwwAuth := rec.Header().Get("WWW-Authenticate")
			if !strings.Contains(wwwAuth, "resource_metadata=") || !strings.Contains(wwwAuth, PRMPath) {
				t.Errorf("WWW-Authenticate %q must reference resource_metadata at %q", wwwAuth, PRMPath)
			}
		})
	}
}

// Test 3 (security-critical): a refresh token, otherwise valid and signed with
// the same key, must be rejected because its `type` is not "access".
func TestMiddleware_RefreshTokenRejected(t *testing.T) {
	f := newFixture(t)
	v := f.readyVerifier(t)

	claims := f.validClaims()
	claims["type"] = "refresh"

	rec, nextCalled := runMiddleware(v, bearer(f.sign(t, claims)))

	if nextCalled {
		t.Errorf("protected handler must not be called for a refresh token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "invalid_token") {
		t.Errorf("expected error=invalid_token in WWW-Authenticate, got %q", rec.Header().Get("WWW-Authenticate"))
	}
}

// Test 4: a token addressed to a different audience must be rejected.
func TestMiddleware_WrongAudience(t *testing.T) {
	f := newFixture(t)
	v := f.readyVerifier(t)

	claims := f.validClaims()
	claims["aud"] = "https://someone-else.test/mcp"

	rec, nextCalled := runMiddleware(v, bearer(f.sign(t, claims)))

	if nextCalled {
		t.Errorf("protected handler must not be called for a wrong-audience token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}
}

// Test 5: an expired token must be rejected.
func TestMiddleware_ExpiredToken(t *testing.T) {
	f := newFixture(t)
	v := f.readyVerifier(t)

	claims := f.validClaims()
	claims["iat"] = jwt.NewNumericDate(time.Now().Add(-2 * time.Hour))
	claims["exp"] = jwt.NewNumericDate(time.Now().Add(-1 * time.Hour))

	rec, nextCalled := runMiddleware(v, bearer(f.sign(t, claims)))

	if nextCalled {
		t.Errorf("protected handler must not be called for an expired token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}
}

// Test 6: the PRM endpoint is unauthenticated, returns the expected JSON, and
// carries permissive CORS headers.
func TestPRMHandler(t *testing.T) {
	f := newFixture(t)
	cfg := Config{Issuer: f.issuer, Audience: f.audience}

	rec := httptest.NewRecorder()
	cfg.PRMHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, PRMPath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("expected CORS allow-origin '*', got %q", got)
	}

	var doc struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		BearerMethods        []string `json:"bearer_methods_supported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshaling PRM body: %v", err)
	}
	if doc.Resource != f.audience {
		t.Errorf("expected resource %q, got %q", f.audience, doc.Resource)
	}
	if len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != f.issuer {
		t.Errorf("expected authorization_servers [%q], got %v", f.issuer, doc.AuthorizationServers)
	}
}

// Test 7 (fail-closed): while signing keys are not yet loaded, a request with an
// otherwise valid token must get 503 and must never reach the protected handler.
func TestMiddleware_KeysNotReady(t *testing.T) {
	f := newFixture(t)
	// Construct a verifier directly with no keyfunc loaded (keys not ready).
	v := &Verifier{cfg: Config{Issuer: f.issuer, Audience: f.audience}}

	rec, nextCalled := runMiddleware(v, bearer(f.sign(t, f.validClaims())))

	if nextCalled {
		t.Errorf("protected handler must not be called while keys are not ready")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", rec.Code)
	}
}

// Test 8 (fail-fast): configuration errors that need no network must cause
// NewVerifier to return an error.
func TestNewVerifier_ConfigErrors(t *testing.T) {
	cases := map[string]Config{
		"issuer not absolute URL": {Issuer: "not-a-url", Audience: "https://rs.test/mcp"},
		"missing audience":        {Issuer: "https://issuer.test", Audience: ""},
		"bad jwks url":            {Issuer: "https://issuer.test", Audience: "https://rs.test/mcp", JWKSURL: "nope"},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewVerifier(context.Background(), cfg); err == nil {
				t.Errorf("expected NewVerifier to return an error for %+v", cfg)
			}
		})
	}
}

// Test 9 (compatibility): a zero-value Config disables authentication.
func TestConfig_Enabled(t *testing.T) {
	if (Config{}).Enabled() {
		t.Errorf("empty Config should report Enabled() == false")
	}
	if !(Config{Issuer: "https://issuer.test"}).Enabled() {
		t.Errorf("Config with issuer should report Enabled() == true")
	}
}

// Discovery: NewVerifier with issuer only must resolve the JWKS URL from the
// OIDC discovery document and become ready.
func TestNewVerifier_Discovery(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	v, err := NewVerifier(ctx, Config{Issuer: f.issuer, Audience: f.audience})
	if err != nil {
		t.Fatalf("NewVerifier with discovery: %v", err)
	}
	if _, ready := v.keyfunc(); !ready {
		t.Fatalf("verifier should be ready after successful discovery")
	}

	rec, nextCalled := runMiddleware(v, bearer(f.sign(t, f.validClaims())))
	if !nextCalled || rec.Code != http.StatusOK {
		t.Errorf("expected discovery-configured verifier to accept a valid token, got status %d (nextCalled=%v)", rec.Code, nextCalled)
	}
}

func withHeader(req *http.Request, key, value string) *http.Request {
	req.Header.Set(key, value)
	return req
}
