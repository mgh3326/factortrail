package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testTeam  = "example-team.cloudflareaccess.com"
	testAUD   = "test-aud-0123456789abcdef"
	testEmail = "operator@example.com"
	testCerts = "https://" + testTeam + "/cdn-cgi/access/certs"
)

var testNow = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

// testKey is a 2048-bit RSA key generated once per test binary; generation is
// the slow part of these tests.
var (
	keyOnce  sync.Once
	keyA     *rsa.PrivateKey
	keyB     *rsa.PrivateKey
	keyErr   error
	keyOther *rsa.PrivateKey
)

func testKeys(t *testing.T) (a, b, other *rsa.PrivateKey) {
	t.Helper()
	keyOnce.Do(func() {
		if keyA, keyErr = rsa.GenerateKey(rand.Reader, 2048); keyErr != nil {
			return
		}
		if keyB, keyErr = rsa.GenerateKey(rand.Reader, 2048); keyErr != nil {
			return
		}
		keyOther, keyErr = rsa.GenerateKey(rand.Reader, 2048)
	})
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	return keyA, keyB, keyOther
}

// jwksTransport stands in for the network: it serves one JWKS document for
// the Access certs URL and records every request. Any other URL fails the
// test, which is how the "only outbound call is JWKS" rule is enforced here.
type jwksTransport struct {
	t *testing.T

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fail      bool
	calls     int
	cancelled int // fetches whose request context was already done
}

func (j *jwksTransport) set(keys map[string]*rsa.PublicKey, fail bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.keys, j.fail = keys, fail
}

func (j *jwksTransport) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.calls
}

func (j *jwksTransport) cancelledFetches() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cancelled
}

func (j *jwksTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.calls++
	if r.Context().Err() != nil {
		j.cancelled++
		return nil, r.Context().Err()
	}
	if r.URL.String() != testCerts {
		j.t.Errorf("unexpected outbound request to %s", r.URL)
		return nil, errors.New("unexpected URL")
	}
	if j.fail {
		return nil, errors.New("network down")
	}
	type jwkDoc struct {
		Kid string `json:"kid"`
		Kty string `json:"kty"`
		N   string `json:"n"`
		E   string `json:"e"`
	}
	var doc struct {
		Keys []jwkDoc `json:"keys"`
	}
	for kid, pub := range j.keys {
		doc.Keys = append(doc.Keys, jwkDoc{
			Kid: kid, Kty: "RSA",
			N: base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		})
	}
	body, _ := json.Marshal(doc)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(body))),
		Request:    r,
	}, nil
}

// harness wires a verifier to a counting handler. The counter is the proof
// that a rejected request never reached the code behind the middleware.
type harness struct {
	t       *testing.T
	jwks    *jwksTransport
	clock   *time.Time
	v       *Verifier
	handler http.Handler

	mu       sync.Mutex
	reached  int
	identity Identity
	authz    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	a, b, _ := testKeys(t)
	h := &harness{t: t}
	h.jwks = &jwksTransport{t: t}
	h.jwks.set(map[string]*rsa.PublicKey{"kid-a": &a.PublicKey, "kid-b": &b.PublicKey}, false)
	now := testNow
	h.clock = &now
	v, err := NewVerifier(Config{
		TeamDomain:    testTeam,
		AUD:           testAUD,
		AllowedEmails: []string{"Operator@Example.com", " other@example.com "},
		HTTPClient:    &http.Client{Transport: h.jwks},
		Now:           func() time.Time { return *h.clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.v = v
	h.handler = v.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.reached++
		h.identity, _ = IdentityFrom(r.Context())
		h.authz = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	return h
}

func (h *harness) reachedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reached
}

func (h *harness) do(headers map[string]string) *httptest.ResponseRecorder {
	h.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

// claims returns a valid claim set; tests mutate one field at a time.
func claims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss":   "https://" + testTeam,
		"aud":   []string{testAUD},
		"email": testEmail,
		"iat":   testNow.Add(-time.Minute).Unix(),
		"nbf":   testNow.Add(-time.Minute).Unix(),
		"exp":   testNow.Add(time.Hour).Unix(),
	}
}

func sign(t *testing.T, method jwt.SigningMethod, kid string, c jwt.MapClaims, key any) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, c)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func assertion(t *testing.T, c jwt.MapClaims) string {
	a, _, _ := testKeys(t)
	return sign(t, jwt.SigningMethodRS256, "kid-a", c, a)
}

func TestRequireRejectsBeforeHandler(t *testing.T) {
	a, _, other := testKeys(t)
	mutate := func(f func(jwt.MapClaims)) string {
		c := claims()
		f(c)
		return assertion(t, c)
	}

	tests := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"missing assertion", nil, http.StatusUnauthorized},
		{"empty assertion", map[string]string{assertionHeader: "  "}, http.StatusUnauthorized},
		{"bearer only", map[string]string{"Authorization": "Bearer anything"}, http.StatusUnauthorized},
		{"plain email header is not an identity", map[string]string{"Cf-Access-Authenticated-User-Email": testEmail}, http.StatusUnauthorized},
		{"garbage", map[string]string{assertionHeader: "not-a-jwt"}, http.StatusUnauthorized},
		{"bad signature", map[string]string{assertionHeader: sign(t, jwt.SigningMethodRS256, "kid-a", claims(), other)}, http.StatusUnauthorized},
		{"alg none", map[string]string{assertionHeader: sign(t, jwt.SigningMethodNone, "kid-a", claims(), jwt.UnsafeAllowNoneSignatureType)}, http.StatusUnauthorized},
		{"alg HS256 keyed with the public key", map[string]string{assertionHeader: sign(t, jwt.SigningMethodHS256, "kid-a", claims(), []byte(a.PublicKey.N.String()))}, http.StatusUnauthorized},
		{"alg RS384", map[string]string{assertionHeader: sign(t, jwt.SigningMethodRS384, "kid-a", claims(), a)}, http.StatusUnauthorized},
		{"alg PS256", map[string]string{assertionHeader: sign(t, jwt.SigningMethodPS256, "kid-a", claims(), a)}, http.StatusUnauthorized},
		{"missing kid", map[string]string{assertionHeader: sign(t, jwt.SigningMethodRS256, "", claims(), a)}, http.StatusUnauthorized},
		{"wrong issuer", map[string]string{assertionHeader: mutate(func(c jwt.MapClaims) { c["iss"] = "https://evil.example.com" })}, http.StatusUnauthorized},
		{"missing issuer", map[string]string{assertionHeader: mutate(func(c jwt.MapClaims) { delete(c, "iss") })}, http.StatusUnauthorized},
		{"wrong audience", map[string]string{assertionHeader: mutate(func(c jwt.MapClaims) { c["aud"] = []string{"other-aud"} })}, http.StatusUnauthorized},
		{"missing audience", map[string]string{assertionHeader: mutate(func(c jwt.MapClaims) { delete(c, "aud") })}, http.StatusUnauthorized},
		{"expired", map[string]string{assertionHeader: mutate(func(c jwt.MapClaims) { c["exp"] = testNow.Add(-time.Hour).Unix() })}, http.StatusUnauthorized},
		{"missing exp", map[string]string{assertionHeader: mutate(func(c jwt.MapClaims) { delete(c, "exp") })}, http.StatusUnauthorized},
		{"not yet valid", map[string]string{assertionHeader: mutate(func(c jwt.MapClaims) { c["nbf"] = testNow.Add(time.Hour).Unix() })}, http.StatusUnauthorized},
		{"issued in the future", map[string]string{assertionHeader: mutate(func(c jwt.MapClaims) { c["iat"] = testNow.Add(time.Hour).Unix() })}, http.StatusUnauthorized},
		{"email not in allowlist", map[string]string{assertionHeader: mutate(func(c jwt.MapClaims) { c["email"] = "intruder@example.com" })}, http.StatusForbidden},
		{"email is a prefix of an allowed one", map[string]string{assertionHeader: mutate(func(c jwt.MapClaims) { c["email"] = "operator@example.co" })}, http.StatusForbidden},
		{"no email claim (service token identity)", map[string]string{assertionHeader: mutate(func(c jwt.MapClaims) { delete(c, "email"); c["common_name"] = "svc.access" })}, http.StatusForbidden},
		{"non-string email", map[string]string{assertionHeader: mutate(func(c jwt.MapClaims) { c["email"] = []string{testEmail} })}, http.StatusForbidden},
		{"bad assertion with a bearer present: no bearer fallback", map[string]string{
			assertionHeader: mutate(func(c jwt.MapClaims) { c["aud"] = []string{"other-aud"} }),
			"Authorization": "Bearer anything",
		}, http.StatusUnauthorized},
		{"expired assertion with a bearer present", map[string]string{
			assertionHeader: mutate(func(c jwt.MapClaims) { c["exp"] = testNow.Add(-time.Hour).Unix() }),
			"Authorization": "Bearer anything",
		}, http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			w := h.do(tt.headers)
			if w.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %q)", w.Code, tt.want, w.Body.String())
			}
			if n := h.reachedCount(); n != 0 {
				t.Fatalf("handler reached %d times for a rejected request", n)
			}
			if got := w.Body.String(); strings.Contains(got, testEmail) || strings.Contains(got, testAUD) {
				t.Fatalf("response leaks configuration: %q", got)
			}
		})
	}
}

func TestRequireRejectsRepeatedAssertionHeader(t *testing.T) {
	h := newHarness(t)
	good := assertion(t, claims())
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Add(assertionHeader, good)
	r.Header.Add(assertionHeader, good)
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || h.reachedCount() != 0 {
		t.Fatalf("status = %d reached = %d, want 401 and 0", w.Code, h.reachedCount())
	}
}

func TestRequireAcceptsValidAssertion(t *testing.T) {
	h := newHarness(t)
	w := h.do(map[string]string{assertionHeader: assertion(t, claims())})
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %q)", w.Code, w.Body.String())
	}
	if h.reachedCount() != 1 {
		t.Fatalf("handler reached %d times, want 1", h.reachedCount())
	}
	if h.identity.Email != testEmail {
		t.Fatalf("identity = %q, want %q", h.identity.Email, testEmail)
	}
}

func TestRequireAllowlistIsCaseInsensitiveAndTrimmed(t *testing.T) {
	h := newHarness(t)
	for _, email := range []string{"OPERATOR@example.COM", "other@example.com"} {
		c := claims()
		c["email"] = email
		if w := h.do(map[string]string{assertionHeader: assertion(t, c)}); w.Code != http.StatusNoContent {
			t.Errorf("email %q: status = %d, want 204", email, w.Code)
		}
	}
}

// A valid assertion decides the request. The bearer value is neither needed
// nor forwarded: nothing behind the middleware may see it.
func TestRequireValidAssertionIgnoresAndStripsBearer(t *testing.T) {
	h := newHarness(t)
	w := h.do(map[string]string{assertionHeader: assertion(t, claims()), "Authorization": "Bearer anything"})
	if w.Code != http.StatusNoContent || h.reachedCount() != 1 {
		t.Fatalf("status = %d reached = %d, want 204 and 1", w.Code, h.reachedCount())
	}
	if h.authz != "" {
		t.Fatalf("downstream handler saw Authorization %q", h.authz)
	}
}

func TestRequireAcceptsStringAudience(t *testing.T) {
	h := newHarness(t)
	c := claims()
	c["aud"] = testAUD
	if w := h.do(map[string]string{assertionHeader: assertion(t, c)}); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
}

func TestRequireClockSkew(t *testing.T) {
	// Within the skew: accepted. Beyond it: rejected.
	tests := []struct {
		name  string
		claim string
		delta time.Duration
		want  int
	}{
		{"exp just past, within skew", "exp", -10 * time.Second, http.StatusNoContent},
		{"exp past beyond skew", "exp", -2 * time.Minute, http.StatusUnauthorized},
		{"nbf slightly ahead, within skew", "nbf", 10 * time.Second, http.StatusNoContent},
		{"nbf ahead beyond skew", "nbf", 2 * time.Minute, http.StatusUnauthorized},
		{"iat slightly ahead, within skew", "iat", 10 * time.Second, http.StatusNoContent},
		{"iat ahead beyond skew", "iat", 2 * time.Minute, http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			c := claims()
			c[tt.claim] = testNow.Add(tt.delta).Unix()
			if w := h.do(map[string]string{assertionHeader: assertion(t, c)}); w.Code != tt.want {
				t.Fatalf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

// JWKS behavior. A request whose key cannot be established from a fresh
// fetch is refused; there is no stale-key fallback.

func TestUnknownKidWithRefreshFailureIsRefused(t *testing.T) {
	h := newHarness(t)
	_, b, _ := testKeys(t)
	h.jwks.set(nil, true)
	tok := sign(t, jwt.SigningMethodRS256, "kid-never-seen", claims(), b)
	w := h.do(map[string]string{assertionHeader: tok})
	if w.Code != http.StatusUnauthorized || h.reachedCount() != 0 {
		t.Fatalf("status = %d reached = %d, want 401 and 0", w.Code, h.reachedCount())
	}
	if h.jwks.count() == 0 {
		t.Fatal("verifier never tried to refresh the key set")
	}
}

func TestUnknownKidAfterWarmCacheWithRefreshFailureIsRefused(t *testing.T) {
	h := newHarness(t)
	a, _, _ := testKeys(t)
	if w := h.do(map[string]string{assertionHeader: assertion(t, claims())}); w.Code != http.StatusNoContent {
		t.Fatalf("warm-up status = %d, want 204", w.Code)
	}
	h.jwks.set(nil, true)
	*h.clock = h.clock.Add(time.Minute) // past the unknown-kid refresh interval, inside the TTL
	tok := sign(t, jwt.SigningMethodRS256, "kid-rotated", claims(), a)
	before := h.reachedCount()
	if w := h.do(map[string]string{assertionHeader: tok}); w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if h.reachedCount() != before {
		t.Fatal("handler reached for an unknown kid")
	}
}

func TestStaleCacheWithRefreshFailureIsRefusedEvenForKnownKid(t *testing.T) {
	h := newHarness(t)
	if w := h.do(map[string]string{assertionHeader: assertion(t, claims())}); w.Code != http.StatusNoContent {
		t.Fatalf("warm-up status = %d, want 204", w.Code)
	}
	h.jwks.set(nil, true)
	*h.clock = h.clock.Add(DefaultCacheTTL + time.Second)
	c := claims()
	c["exp"] = h.clock.Add(time.Hour).Unix()
	c["iat"] = h.clock.Add(-time.Minute).Unix()
	c["nbf"] = h.clock.Add(-time.Minute).Unix()
	before := h.reachedCount()
	if w := h.do(map[string]string{assertionHeader: assertion(t, c)}); w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (stale keys must not be used)", w.Code)
	}
	if h.reachedCount() != before {
		t.Fatal("handler reached with stale keys")
	}
}

func TestStaleCacheRefreshesWhenJWKSIsBack(t *testing.T) {
	h := newHarness(t)
	h.do(map[string]string{assertionHeader: assertion(t, claims())})
	*h.clock = h.clock.Add(DefaultCacheTTL + time.Second)
	c := claims()
	c["exp"] = h.clock.Add(time.Hour).Unix()
	c["iat"] = h.clock.Add(-time.Minute).Unix()
	c["nbf"] = h.clock.Add(-time.Minute).Unix()
	if w := h.do(map[string]string{assertionHeader: assertion(t, c)}); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if h.jwks.count() != 2 {
		t.Fatalf("JWKS fetches = %d, want 2", h.jwks.count())
	}
}

func TestKeyRotationIsPickedUp(t *testing.T) {
	h := newHarness(t)
	a, _, _ := testKeys(t)
	h.do(map[string]string{assertionHeader: assertion(t, claims())})
	// Cloudflare rotates: kid-c appears, kid-b and kid-a leave.
	h.jwks.set(map[string]*rsa.PublicKey{"kid-c": &a.PublicKey}, false)
	*h.clock = h.clock.Add(time.Minute)
	tok := sign(t, jwt.SigningMethodRS256, "kid-c", claims(), a)
	if w := h.do(map[string]string{assertionHeader: tok}); w.Code != http.StatusNoContent {
		t.Fatalf("new kid status = %d, want 204", w.Code)
	}
	// The retired kid is gone after the refresh: no lingering old keys.
	if w := h.do(map[string]string{assertionHeader: assertion(t, claims())}); w.Code != http.StatusUnauthorized {
		t.Fatalf("retired kid status = %d, want 401", w.Code)
	}
}

// Unknown kids cannot be used to make the verifier hammer the JWKS endpoint.
func TestUnknownKidRefreshIsRateLimited(t *testing.T) {
	h := newHarness(t)
	_, b, _ := testKeys(t)
	h.do(map[string]string{assertionHeader: assertion(t, claims())}) // 1 fetch
	for i := 0; i < 20; i++ {
		tok := sign(t, jwt.SigningMethodRS256, "kid-random", claims(), b)
		h.do(map[string]string{assertionHeader: tok})
	}
	if n := h.jwks.count(); n > 2 {
		t.Fatalf("JWKS fetches = %d after 20 unknown-kid requests, want at most 2", n)
	}
}

func TestFailedRefreshIsNotRetriedOnEveryRequest(t *testing.T) {
	h := newHarness(t)
	h.jwks.set(nil, true)
	for i := 0; i < 10; i++ {
		h.do(map[string]string{assertionHeader: assertion(t, claims())})
	}
	if n := h.jwks.count(); n != 1 {
		t.Fatalf("JWKS fetches = %d during an outage inside the retry interval, want 1", n)
	}
}

func TestJWKSDocumentValidation(t *testing.T) {
	a, _, _ := testKeys(t)
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		body string
		ctyp string
		code int
	}{
		{"not json content type", `{"keys":[]}`, "text/html", 200},
		{"redirect status", ``, "application/json", 302},
		{"server error", ``, "application/json", 500},
		{"no keys", `{"keys":[]}`, "application/json", 200},
		{"non-rsa key", `{"keys":[{"kid":"kid-a","kty":"EC"}]}`, "application/json", 200},
		{"1024-bit key is too small", `{"keys":[{"kid":"kid-a","kty":"RSA","n":"` + base64.RawURLEncoding.EncodeToString(small.N.Bytes()) + `","e":"AQAB"}]}`, "application/json", 200},
		{"truncated json", `{"keys":[`, "application/json", 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: tt.code,
					Header:     http.Header{"Content-Type": []string{tt.ctyp}},
					Body:       io.NopCloser(strings.NewReader(tt.body)),
					Request:    r,
				}, nil
			})
			v, err := NewVerifier(Config{TeamDomain: testTeam, AUD: testAUD, AllowedEmails: []string{testEmail}, HTTPClient: &http.Client{Transport: rt}, Now: func() time.Time { return testNow }})
			if err != nil {
				t.Fatal(err)
			}
			reached := 0
			srv := v.Require(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }))
			r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			r.Header.Set(assertionHeader, sign(t, jwt.SigningMethodRS256, "kid-a", claims(), a))
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized || reached != 0 {
				t.Fatalf("status = %d reached = %d, want 401 and 0", w.Code, reached)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDefaultClientDoesNotFollowRedirects(t *testing.T) {
	// The JWKS call is the only outbound request; a redirect must not be
	// able to point it somewhere else.
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("redirect target was contacted")
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer srv.Close()
	c := defaultHTTPClient()
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want the 302 returned unfollowed", resp.StatusCode)
	}
}

func TestNewVerifierRefusesIncompleteConfig(t *testing.T) {
	ok := Config{TeamDomain: testTeam, AUD: testAUD, AllowedEmails: []string{testEmail}}
	tests := []struct {
		name string
		edit func(*Config)
	}{
		{"no team domain", func(c *Config) { c.TeamDomain = "" }},
		{"no aud", func(c *Config) { c.AUD = " " }},
		{"no emails", func(c *Config) { c.AllowedEmails = nil }},
		{"blank emails only", func(c *Config) { c.AllowedEmails = []string{"", "  "} }},
		{"email without @", func(c *Config) { c.AllowedEmails = []string{"operator"} }},
		{"team domain with scheme", func(c *Config) { c.TeamDomain = "https://" + testTeam }},
		{"team domain with path", func(c *Config) { c.TeamDomain = testTeam + "/x" }},
		{"team domain with port", func(c *Config) { c.TeamDomain = testTeam + ":8443" }},
		{"team domain with userinfo", func(c *Config) { c.TeamDomain = "a@" + testTeam }},
		{"team domain with space", func(c *Config) { c.TeamDomain = "a b.example.com" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := ok
			tt.edit(&c)
			if _, err := NewVerifier(c); err == nil {
				t.Fatal("NewVerifier accepted an invalid configuration")
			}
		})
	}
	if _, err := NewVerifier(ok); err != nil {
		t.Fatalf("valid configuration refused: %v", err)
	}
}

// A failed refresh for an unknown kid must not take down keys that are still
// within their TTL: the unknown key is refused, valid assertions are not.
func TestFailedUnknownKidRefreshKeepsFreshKeys(t *testing.T) {
	h := newHarness(t)
	_, b, _ := testKeys(t)
	if w := h.do(map[string]string{assertionHeader: assertion(t, claims())}); w.Code != http.StatusNoContent {
		t.Fatalf("warm-up status = %d, want 204", w.Code)
	}
	h.jwks.set(nil, true)
	*h.clock = h.clock.Add(time.Minute) // past the retry interval, inside the TTL
	unknown := sign(t, jwt.SigningMethodRS256, "kid-never-seen", claims(), b)
	if w := h.do(map[string]string{assertionHeader: unknown}); w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown kid status = %d, want 401", w.Code)
	}
	c := claims()
	c["iat"] = h.clock.Add(-time.Minute).Unix()
	c["nbf"] = h.clock.Add(-time.Minute).Unix()
	c["exp"] = h.clock.Add(time.Hour).Unix()
	if w := h.do(map[string]string{assertionHeader: assertion(t, c)}); w.Code != http.StatusNoContent {
		t.Fatalf("valid assertion after a failed unknown-kid refresh: status = %d, want 204", w.Code)
	}
}

// The shared key set is refreshed on its own context: a client that
// disconnects mid-request cannot make the refresh fail for everyone.
func TestRefreshIsNotCancelledByTheRequest(t *testing.T) {
	h := newHarness(t)
	a, _, _ := testKeys(t)
	h.do(map[string]string{assertionHeader: assertion(t, claims())})
	h.jwks.set(map[string]*rsa.PublicKey{"kid-a": &a.PublicKey, "kid-c": &a.PublicKey}, false)
	*h.clock = h.clock.Add(time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/mcp", nil)
	r.Header.Set(assertionHeader, sign(t, jwt.SigningMethodRS256, "kid-c", claims(), a))
	h.handler.ServeHTTP(httptest.NewRecorder(), r)

	if h.jwks.cancelledFetches() != 0 {
		t.Fatalf("%d JWKS fetches ran on a cancelled request context", h.jwks.cancelledFetches())
	}
	// The refresh completed, so the rotated key is now cached.
	if w := h.do(map[string]string{assertionHeader: sign(t, jwt.SigningMethodRS256, "kid-c", claims(), a)}); w.Code != http.StatusNoContent {
		t.Fatalf("rotated kid after a cancelled request: status = %d, want 204", w.Code)
	}
}
