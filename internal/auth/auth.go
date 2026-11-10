// Package auth verifies Cloudflare Access assertions and gates HTTP routes on
// them.
//
// The verifier is stricter than the code it grew from, on purpose:
//   - an assertion that is present must verify; there is no second credential
//     to fall back to (inbound bearer tokens do not exist in P0);
//   - the JWKS cache has no stale-key fallback: when the key set cannot be
//     refreshed, the request is refused;
//   - only RS256 is accepted, and the identity is the verified email claim,
//     never a plain header.
//
// The JWKS parsing and the overall shape of the key cache are adapted from
// github.com/mgh3326/handoffkeep at commit 4f45717f5cf451dc5b6e535585164c073b0803f8
// (internal/cfaccess/cfaccess.go), MIT License, Copyright (c) 2026 Kwanghyun Moon.
// Permission is granted under the MIT License terms reproduced in that
// repository's LICENSE file, which this repository's LICENSE matches. The
// changes are: no service-name identities, no stale-key window, no background
// refresh, a bounded response body, a minimum RSA key size and an explicit
// minimum interval between refresh attempts.
package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const assertionHeader = "Cf-Access-Jwt-Assertion"

const (
	// DefaultCacheTTL bounds how long a fetched key set is trusted.
	DefaultCacheTTL = 10 * time.Minute
	// minRefreshInterval is the shortest gap between two JWKS fetch attempts,
	// successful or not. It stops unknown key IDs and outages from turning
	// every request into an outbound call.
	minRefreshInterval = 30 * time.Second
	// leeway is the clock skew tolerated on exp, nbf and iat.
	leeway = 30 * time.Second

	fetchTimeout = 3 * time.Second
	maxJWKSBytes = 1 << 20
	maxJWKSKeys  = 32
	minRSABits   = 2048
)

// Verification failures. Callers map them to 401 and 403 and never expose
// more detail than that to the client.
var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
)

// Config configures a Verifier. HTTPClient and Now exist so tests can run
// without a network or a wall clock; production leaves both nil.
type Config struct {
	// TeamDomain is the Access team host name, for example
	// "example.cloudflareaccess.com". It is the token issuer
	// ("https://"+TeamDomain) and the JWKS host.
	TeamDomain string
	// AUD is the Access application audience tag.
	AUD string
	// AllowedEmails is the exact, case-insensitive allowlist of operators.
	AllowedEmails []string

	HTTPClient *http.Client
	Now        func() time.Time
	Logger     *slog.Logger
}

// Identity is the verified principal of a request.
type Identity struct {
	Email string
}

type identityKey struct{}

// IdentityFrom returns the identity that Require attached to ctx.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(Identity)
	return id, ok
}

// Verifier checks Access assertions against the team's published keys.
type Verifier struct {
	issuer  string
	aud     string
	certs   string
	allowed map[string]struct{}
	client  *http.Client
	now     func() time.Time
	log     *slog.Logger
	parser  *jwt.Parser

	mu          sync.Mutex // held across a fetch: concurrent refreshes collapse into one
	keys        map[string]*rsa.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
}

var teamDomainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// NewVerifier validates cfg and returns a fail-closed verifier. An incomplete
// configuration is an error, never a disabled verifier.
func NewVerifier(cfg Config) (*Verifier, error) {
	team := asciiLower(strings.TrimSpace(cfg.TeamDomain))
	if team == "" {
		return nil, errors.New("access team domain is empty")
	}
	if !teamDomainRE.MatchString(team) {
		return nil, errors.New("access team domain must be a bare host name such as example.cloudflareaccess.com")
	}
	aud := strings.TrimSpace(cfg.AUD)
	if aud == "" {
		return nil, errors.New("access audience is empty")
	}
	allowed := make(map[string]struct{}, len(cfg.AllowedEmails))
	for _, e := range cfg.AllowedEmails {
		e = asciiLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !validEmail(e) {
			return nil, errors.New("access email allowlist contains an entry that is not an email address")
		}
		allowed[e] = struct{}{}
	}
	if len(allowed) == 0 {
		return nil, errors.New("access email allowlist is empty")
	}

	v := &Verifier{
		issuer:  "https://" + team,
		aud:     aud,
		certs:   "https://" + team + "/cdn-cgi/access/certs",
		allowed: allowed,
		client:  cfg.HTTPClient,
		now:     cfg.Now,
		log:     cfg.Logger,
	}
	if v.client == nil {
		v.client = defaultHTTPClient()
	}
	if v.now == nil {
		v.now = time.Now
	}
	if v.log == nil {
		v.log = slog.New(slog.DiscardHandler)
	}
	v.parser = jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.aud),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(leeway),
		jwt.WithTimeFunc(v.now),
	)
	return v, nil
}

// defaultHTTPClient is the only outbound HTTP client in the service. It does
// not follow redirects, so the JWKS call cannot be steered to another host.
func defaultHTTPClient() *http.Client {
	return &http.Client{
		Timeout: fetchTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Verify checks one assertion. The error wraps ErrUnauthorized when the
// assertion is missing or invalid and ErrForbidden when it is valid for this
// application but its email is not on the allowlist.
func (v *Verifier) Verify(ctx context.Context, assertion string) (Identity, error) {
	if assertion == "" {
		return Identity{}, fmt.Errorf("%w: no assertion", ErrUnauthorized)
	}
	claims := jwt.MapClaims{}
	_, err := v.parser.ParseWithClaims(assertion, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if strings.TrimSpace(kid) == "" {
			return nil, errors.New("assertion has no key id")
		}
		return v.keyFor(ctx, kid)
	})
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	email, _ := claims["email"].(string)
	email = strings.TrimSpace(email)
	if email == "" {
		// A service-token assertion carries no email; it is not an operator.
		return Identity{}, fmt.Errorf("%w: assertion has no email claim", ErrForbidden)
	}
	if _, ok := v.allowed[asciiLower(email)]; !ok {
		return Identity{}, fmt.Errorf("%w: email is not on the allowlist", ErrForbidden)
	}
	return Identity{Email: email}, nil
}

// Require wraps next so that only requests with a valid assertion reach it.
// It works for any route. The Authorization header is removed from the
// request next sees: the assertion is the only credential, and no bearer
// value may be mistaken for one downstream.
func (v *Verifier) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values(assertionHeader)
		if len(values) != 1 {
			v.reject(w, r, fmt.Errorf("%w: expected exactly one assertion header, got %d", ErrUnauthorized, len(values)))
			return
		}
		id, err := v.Verify(r.Context(), strings.TrimSpace(values[0]))
		if err != nil {
			v.reject(w, r, err)
			return
		}
		r2 := r.WithContext(context.WithValue(r.Context(), identityKey{}, id))
		r2.Header = r.Header.Clone()
		r2.Header.Del("Authorization")
		next.ServeHTTP(w, r2)
	})
}

func (v *Verifier) reject(w http.ResponseWriter, r *http.Request, err error) {
	status, body := http.StatusUnauthorized, `{"error":"unauthorized"}`
	if errors.Is(err, ErrForbidden) {
		status, body = http.StatusForbidden, `{"error":"forbidden"}`
	}
	v.log.Warn("auth.rejected", "status", status, "path", r.URL.Path, "reason", err.Error())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// keyFor returns the public key for kid from a key set no older than the
// cache TTL. It fetches a new set when the cached one is stale or does not
// contain kid, at most once per minRefreshInterval. When a needed fetch is
// not possible the answer is an error: stale keys are dropped, never reused.
func (v *Verifier) keyFor(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	now := v.now()
	if v.keys != nil && now.Sub(v.fetchedAt) < DefaultCacheTTL {
		if key, ok := v.keys[kid]; ok {
			return key, nil
		}
	}
	if !v.lastAttempt.IsZero() && now.Sub(v.lastAttempt) < minRefreshInterval {
		return nil, errors.New("signing key not available")
	}
	v.lastAttempt = now
	keys, err := v.fetch(ctx)
	if err != nil {
		v.keys = nil
		return nil, fmt.Errorf("refresh signing keys: %w", err)
	}
	v.keys, v.fetchedAt = keys, now
	key, ok := keys[kid]
	if !ok {
		return nil, errors.New("unknown signing key")
	}
	return key, nil
}

func (v *Verifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.certs, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS request returned status %d", resp.StatusCode)
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return nil, errors.New("JWKS response was not JSON")
	}
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode JWKS: %w", err)
	}
	if len(doc.Keys) > maxJWKSKeys {
		return nil, errors.New("JWKS holds too many keys")
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, raw := range doc.Keys {
		if key, ok := raw.rsaKey(); ok {
			keys[raw.Kid] = key
		}
	}
	if len(keys) == 0 {
		return nil, errors.New("JWKS contains no usable RSA keys")
	}
	return keys, nil
}

type jwk struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (j jwk) rsaKey() (*rsa.PublicKey, bool) {
	if j.Kty != "RSA" || j.Kid == "" || j.N == "" || j.E == "" {
		return nil, false
	}
	n, err := base64.RawURLEncoding.DecodeString(j.N)
	if err != nil || len(n) == 0 {
		return nil, false
	}
	e, err := base64.RawURLEncoding.DecodeString(j.E)
	if err != nil || len(e) == 0 || len(e) > 8 {
		return nil, false
	}
	exponent := 0
	for _, b := range e {
		exponent = exponent<<8 | int(b)
	}
	if exponent < 3 {
		return nil, false
	}
	modulus := new(big.Int).SetBytes(n)
	if modulus.BitLen() < minRSABits {
		return nil, false
	}
	return &rsa.PublicKey{N: modulus, E: exponent}, true
}

func validEmail(e string) bool {
	at := strings.IndexByte(e, '@')
	return at > 0 && at < len(e)-1 && strings.Count(e, "@") == 1 && !strings.ContainsAny(e, " \t\r\n,")
}

// asciiLower folds only A-Z. strings.ToLower would also fold characters such
// as the Kelvin sign into ASCII letters, which an exact-match allowlist must
// not do.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
