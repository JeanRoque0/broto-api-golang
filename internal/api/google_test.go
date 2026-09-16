package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

func googleTestConfig() Config {
	return Config{GoogleEnabled: true, GoogleClientID: "test.apps.googleusercontent.com", GoogleClientSecret: "test-only-secret",
		GoogleRedirectURL: "https://api.example.invalid/v1/auth/google/callback", GoogleReturnURLs: "broto://auth-callback,https://app.example.invalid/auth-callback"}
}

func TestGoogleConfigAndPKCE(t *testing.T) {
	for _, tc := range []struct {
		name    string
		edit    func(*Config)
		invalid bool
	}{
		{"valid", func(*Config) {}, false},
		{"disabled", func(c *Config) { *c = Config{} }, false},
		{"missing ID", func(c *Config) { c.GoogleClientID = "" }, true},
		{"multiple IDs", func(c *Config) { c.GoogleClientID = "one.apps.googleusercontent.com,two.apps.googleusercontent.com" }, true},
		{"missing secret", func(c *Config) { c.GoogleClientSecret = " " }, true},
		{"HTTP localhost", func(c *Config) { c.GoogleRedirectURL = "http://localhost:8080/v1/auth/google/callback" }, false},
		{"HTTP remote", func(c *Config) { c.GoogleRedirectURL = "http://api.example.invalid/v1/auth/google/callback" }, true},
		{"callback credentials", func(c *Config) { c.GoogleRedirectURL = "https://user:pass@api.example.invalid/v1/auth/google/callback" }, true},
		{"wrong callback path", func(c *Config) { c.GoogleRedirectURL = "https://api.example.invalid/callback" }, true},
		{"callback fragment", func(c *Config) { c.GoogleRedirectURL += "#fragment" }, true},
		{"callback query", func(c *Config) { c.GoogleRedirectURL += "?x=y" }, true},
		{"no returns", func(c *Config) { c.GoogleReturnURLs = "" }, true},
		{"return query", func(c *Config) { c.GoogleReturnURLs = "broto://auth-callback?next=evil" }, true},
		{"return userinfo", func(c *Config) { c.GoogleReturnURLs = "https://user@app.example.invalid/callback" }, true},
		{"return scheme", func(c *Config) { c.GoogleReturnURLs = "javascript:alert(1)" }, true},
		{"return HTTP remote", func(c *Config) { c.GoogleReturnURLs = "http://app.example.invalid/callback" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := googleTestConfig()
			tc.edit(&c)
			if e := validateGoogleConfig(c); (e != nil) != tc.invalid {
				t.Fatalf("error=%v invalid=%v", e, tc.invalid)
			}
		})
	}
	// Published RFC 7636 example, independent of our implementation.
	v := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	want := "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if challenge(v) != want || !validChallenge(want) || !pkceVerifier.MatchString(v) {
		t.Fatal("PKCE vector mismatch")
	}
	for _, value := range []string{"", "plain", want + "=", strings.Repeat("a", 44), strings.Repeat("/", 43)} {
		if validChallenge(value) {
			t.Fatal("invalid challenge accepted")
		}
	}
	for _, value := range []string{strings.Repeat("a", 42), strings.Repeat("a", 129), strings.Repeat("/", 43)} {
		if pkceVerifier.MatchString(value) {
			t.Fatal("invalid verifier accepted")
		}
	}
}

// The Google transport is simulated, but ID tokens are really signed with RSA
// and verified using the actual OIDC library and a JWKS response. No egress.
type fakeGoogle struct {
	key     *rsa.PrivateKey
	mu      sync.Mutex
	grants  map[string]url.Values
	claims  map[string]any
	failure string
	tokens  int
	keys    int
	client  *http.Client
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	g := &fakeGoogle{key: key, grants: map[string]url.Values{}, claims: map[string]any{}}
	g.client = &http.Client{Transport: transportFunc(g.roundTrip), Timeout: time.Second}
	return g
}

func googleReply(status int, value any) *http.Response {
	b, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(b)))}
}

func (g *fakeGoogle) grant(params url.Values) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	code := randomToken()
	g.grants[code] = params
	return code
}

func (g *fakeGoogle) roundTrip(r *http.Request) (*http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.URL.String() == googleKeysURL {
		g.keys++
		if g.failure == "keys" {
			return googleReply(503, map[string]string{"error": "unavailable"}), nil
		}
		return googleReply(200, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &g.key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"}}}), nil
	}
	if r.URL.String() != "https://oauth2.googleapis.com/token" || r.Method != "POST" {
		return nil, errors.New("unmocked external request blocked")
	}
	g.tokens++
	if e := r.ParseForm(); e != nil {
		return nil, e
	}
	params, ok := g.grants[r.Form.Get("code")]
	if !ok || params.Get("code_challenge") != challenge(r.Form.Get("code_verifier")) || r.Form.Get("client_id") != googleTestConfig().GoogleClientID || r.Form.Get("client_secret") != googleTestConfig().GoogleClientSecret || r.Form.Get("redirect_uri") != googleTestConfig().GoogleRedirectURL || r.Form.Get("grant_type") != "authorization_code" {
		return googleReply(400, map[string]string{"error": "invalid_grant"}), nil
	}
	delete(g.grants, r.Form.Get("code"))
	if g.failure == "network" {
		return nil, errors.New("simulated network failure")
	}
	if g.failure == "token" {
		return googleReply(400, map[string]string{"error": "invalid_grant", "error_description": "private-provider-details"}), nil
	}
	claims := map[string]any{"iss": googleIssuer, "aud": googleTestConfig().GoogleClientID, "sub": "google-subject-123", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": params.Get("nonce"), "email": "garden@example.invalid", "email_verified": true, "name": "Maria"}
	for k, v := range g.claims {
		claims[k] = v
	}
	key := g.key
	if g.failure == "signature" {
		var e error
		key, e = rsa.GenerateKey(rand.Reader, 2048)
		if e != nil {
			return nil, e
		}
	}
	signer, e := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key"))
	if e != nil {
		return nil, e
	}
	b, _ := json.Marshal(claims)
	signed, e := signer.Sign(b)
	if e != nil {
		return nil, e
	}
	raw, e := signed.CompactSerialize()
	if e != nil {
		return nil, e
	}
	result := map[string]any{"access_token": "unused-google-access-token", "token_type": "Bearer", "expires_in": 3600, "id_token": raw}
	if g.failure == "missing token" {
		delete(result, "id_token")
	}
	if g.failure == "malformed token" {
		result["id_token"] = "not.a.jwt"
	}
	return googleReply(200, result), nil
}

func TestGoogleIDTokenValidation(t *testing.T) {
	for _, tc := range []struct {
		name, failure string
		claims        map[string]any
		invalid       bool
	}{
		{name: "valid"},
		{name: "legacy Google issuer", claims: map[string]any{"iss": "accounts.google.com"}},
		{name: "multiple audiences with authorized party", claims: map[string]any{"aud": []string{"other", googleTestConfig().GoogleClientID}, "azp": googleTestConfig().GoogleClientID}},
		{name: "wrong issuer", claims: map[string]any{"iss": "https://evil.invalid"}, invalid: true},
		{name: "wrong audience", claims: map[string]any{"aud": "other-client"}, invalid: true},
		{name: "expired", claims: map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}, invalid: true},
		{name: "nonce mismatch", claims: map[string]any{"nonce": "other"}, invalid: true},
		{name: "missing nonce", claims: map[string]any{"nonce": ""}, invalid: true},
		{name: "missing subject", claims: map[string]any{"sub": ""}, invalid: true},
		{name: "missing email", claims: map[string]any{"email": ""}, invalid: true},
		{name: "unverified email", claims: map[string]any{"email_verified": false}, invalid: true},
		{name: "email display name", claims: map[string]any{"email": "Name <a@example.invalid>"}, invalid: true},
		{name: "wrong authorized party", claims: map[string]any{"azp": "other"}, invalid: true},
		{name: "multiple audiences missing authorized party", claims: map[string]any{"aud": []string{googleTestConfig().GoogleClientID, "other"}}, invalid: true},
		{name: "forged signature", failure: "signature", invalid: true},
		{name: "JWKS outage", failure: "keys", invalid: true},
		{name: "provider rejection", failure: "token", invalid: true},
		{name: "network failure", failure: "network", invalid: true},
		{name: "missing token", failure: "missing token", invalid: true},
		{name: "malformed token", failure: "malformed token", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeGoogle(t)
			fake.failure = tc.failure
			fake.claims = tc.claims
			g := newGoogleOAuth(googleTestConfig(), fake.client)
			v, nonce := randomToken(), randomToken()
			code := fake.grant(url.Values{"nonce": {nonce}, "code_challenge": {challenge(v)}})
			identity, e := g.identity(context.Background(), code, v, hash(nonce))
			if (e != nil) != tc.invalid {
				t.Fatalf("error=%v invalid=%v", e, tc.invalid)
			}
			if e == nil && (identity.Subject != "google-subject-123" || identity.Email != "garden@example.invalid") {
				t.Fatal(identity)
			}
		})
	}
}

func TestGoogleDisabledAndRateLimit(t *testing.T) {
	s := &Server{limits: map[string]*window{}}
	for _, path := range []string{"/v1/auth/google/start", "/v1/auth/google/authorize", "/v1/auth/google/callback", "/v1/auth/google/exchange"} {
		method := "GET"
		if strings.HasSuffix(path, "start") || strings.HasSuffix(path, "exchange") {
			method = "POST"
		}
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, httptest.NewRequest(method, path, nil))
		if w.Code != 503 {
			t.Fatal(w.Code)
		}
	}
	s.google = &googleOAuth{}
	r := httptest.NewRequest("POST", "/v1/auth/google/start", nil)
	s.limits["google:"+remoteIP(r)] = &window{start: time.Now(), n: 30}
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	if w.Code != 429 {
		t.Fatal(w.Code)
	}
}
