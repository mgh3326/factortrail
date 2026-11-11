package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mgh3326/factortrail/internal/auth"
	"github.com/mgh3326/factortrail/internal/config"
	"github.com/mgh3326/factortrail/internal/mcp"
	"github.com/mgh3326/factortrail/internal/providers/autotrader/fake"
)

// TestMain lets the tests run the real main() in a child process: with
// FACTORTRAIL_TEST_MAIN set, the test binary behaves as the factortrail
// command, so exit codes and stderr are those of the actual process.
func TestMain(m *testing.M) {
	if os.Getenv("FACTORTRAIL_TEST_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

func runProcess(t *testing.T, env []string, args ...string) (code int, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	// The child gets only what the case lists, never the parent's environment.
	cmd.Env = append([]string{"FACTORTRAIL_TEST_MAIN=1"}, env...)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	err := cmd.Run()
	if err == nil {
		return 0, errb.String()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), errb.String()
	}
	t.Fatalf("running child: %v", err)
	return 0, ""
}

var accessEnv = []string{
	config.EnvAccessTeamDomain + "=example-team.cloudflareaccess.com",
	config.EnvAccessAUD + "=aud-tag",
	config.EnvAccessAllowedEmails + "=operator@example.com",
}

// The process must refuse to start, with a clear error and a non-zero
// status, before it opens any listener.
func TestProcessRefusesToStart(t *testing.T) {
	tests := []struct {
		name    string
		env     []string
		args    []string
		wantErr string
	}{
		{"no Access settings in production", nil, []string{"serve", "--profile", "retro"}, config.EnvAccessTeamDomain},
		{"team domain only", accessEnv[:1], []string{"serve", "--profile", "retro"}, config.EnvAccessAUD},
		{"AUD and emails but no team domain", accessEnv[1:], []string{"serve", "--profile", "retro"}, config.EnvAccessTeamDomain},
		{"team domain and AUD but no emails", accessEnv[:2], []string{"serve", "--profile", "retro"}, config.EnvAccessAllowedEmails},
		{"dev with a partial Access set", accessEnv[:1], []string{"serve", "--dev", "--profile", "retro"}, "incomplete Access configuration"},
		{"empty profile", accessEnv, []string{"serve"}, "profile is required"},
		{"empty profile from a blank env var", append([]string{config.EnvProfile + "=  "}, accessEnv...), []string{"serve"}, "profile is required"},
		{"unknown profile", accessEnv, []string{"serve", "--profile", "everything"}, `unknown profile "everything"`},
		{"dev with 0.0.0.0", nil, []string{"serve", "--dev", "--profile", "retro", "--addr", "0.0.0.0:0"}, "loopback"},
		{"dev with an empty host", nil, []string{"serve", "--dev", "--profile", "retro", "--addr", ":0"}, "loopback"},
		{"dev with a LAN address", nil, []string{"serve", "--dev", "--profile", "retro", "--addr", "192.168.0.5:0"}, "loopback"},
		{"malformed team domain", []string{
			config.EnvAccessTeamDomain + "=https://example.com/path",
			config.EnvAccessAUD + "=a", config.EnvAccessAllowedEmails + "=o@example.com",
		}, []string{"serve", "--profile", "retro"}, "team domain"},
		{"malformed public host", accessEnv, []string{"serve", "--profile", "retro", "--public-host", "*.example.com"}, "public host"},
		{"base URL without token file", append([]string{config.EnvRetroBaseURL + "=http://127.0.0.1:8000"}, accessEnv...), []string{"serve", "--profile", "retro"}, config.EnvRetroTokenFile},
		{"token file without base URL", append([]string{config.EnvRetroTokenFile + "=/run/secrets/retro"}, accessEnv...), []string{"serve", "--profile", "retro"}, config.EnvRetroBaseURL},
		{"unreadable token file", append([]string{config.EnvRetroBaseURL + "=http://127.0.0.1:8000", config.EnvRetroTokenFile + "=/nonexistent/dir/token"}, accessEnv...), []string{"serve", "--profile", "retro"}, "token file cannot be read"},
		{"base URL with user info", append([]string{config.EnvRetroBaseURL + "=http://user:pw@127.0.0.1:8000", config.EnvRetroTokenFile + "=/nonexistent/dir/token"}, accessEnv...), []string{"serve", "--profile", "retro"}, "user info"},
		{"unknown subcommand", nil, []string{"frobnicate"}, "usage"},
		{"no subcommand", nil, nil, "usage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stderr := runProcess(t, tt.env, tt.args...)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2 (stderr %q)", code, stderr)
			}
			if !strings.Contains(stderr, tt.wantErr) {
				t.Fatalf("stderr %q does not contain %q", stderr, tt.wantErr)
			}
			if strings.Contains(stderr, "/nonexistent/dir") || strings.Contains(stderr, "pw@") || strings.Contains(stderr, "aud-tag") || strings.Contains(stderr, "operator@example.com") {
				t.Fatalf("stderr leaks a configured value: %q", stderr)
			}
		})
	}
}

func TestVersionPrintsRevision(t *testing.T) {
	var out, errb bytes.Buffer
	prev := revision
	revision = "0123abc"
	defer func() { revision = prev }()
	if code := run(context.Background(), []string{"version"}, nopEnv, &out, &errb, nil); code != 0 {
		t.Fatalf("exit code = %d stderr = %q", code, errb.String())
	}
	if strings.TrimSpace(out.String()) != "0123abc" {
		t.Fatalf("version output = %q", out.String())
	}
}

func nopEnv(string) string { return "" }

func envMap(kv ...string) func(string) string {
	m := map[string]string{}
	for _, e := range kv {
		k, v, _ := strings.Cut(e, "=")
		m[k] = v
	}
	return func(k string) string { return m[k] }
}

// A refused configuration must not reach the listen call.
func TestRefusalHappensBeforeListen(t *testing.T) {
	var listened atomic.Bool
	listen := func(string) (net.Listener, error) {
		listened.Store(true)
		return nil, io.EOF
	}
	for _, args := range [][]string{
		{"serve", "--profile", "retro"},         // no Access
		{"serve", "--profile", "nope", "--dev"}, // unknown profile
		{"serve", "--dev", "--profile", "retro", "--addr", "0.0.0.0:0"},
	} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), args, nopEnv, &out, &errb, listen); code != 2 {
			t.Fatalf("%v: exit code = %d, want 2", args, code)
		}
	}
	if listened.Load() {
		t.Fatal("listen was called for a refused configuration")
	}
}

// A dev server starts on loopback, answers /healthz and shuts down cleanly.
func TestServeDevAndShutdown(t *testing.T) {
	lnCh := make(chan net.Listener, 1)
	listen := func(addr string) (net.Listener, error) {
		ln, err := net.Listen("tcp", addr)
		lnCh <- ln
		return ln, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var out, errb bytes.Buffer
	go func() {
		done <- run(ctx, []string{"serve", "--dev", "--profile", "retro", "--addr", "127.0.0.1:0"}, nopEnv, &out, &errb, listen)
	}()
	ln := <-lnCh
	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(body) != 0 {
		t.Fatalf("healthz status = %d body = %q", resp.StatusCode, body)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code = %d stderr = %q", code, errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop after the context was cancelled")
	}
}

// End-to-end through the real handler stack with a stubbed JWKS endpoint.

const (
	testTeam = "example-team.cloudflareaccess.com"
	testAUD  = "aud-tag"
	testMail = "operator@example.com"
)

type stubJWKS struct{ pub *rsa.PublicKey }

func (s stubJWKS) RoundTrip(r *http.Request) (*http.Response, error) {
	doc, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kid": "k1", "kty": "RSA",
		"n": base64.RawURLEncoding.EncodeToString(s.pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(s.pub.E)).Bytes()),
	}}})
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(doc)), Request: r}, nil
}

type headerTransport struct {
	next    http.RoundTripper
	headers map[string]string
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.headers {
		r.Header.Set(k, v)
	}
	return h.next.RoundTrip(r)
}

func TestEndToEndThroughAuth(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.NewVerifier(auth.Config{
		TeamDomain: testTeam, AUD: testAUD, AllowedEmails: []string{testMail},
		HTTPClient: &http.Client{Transport: stubJWKS{&key.PublicKey}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var reached atomic.Int32
	inner, err := mcp.NewHandler(mcp.Options{Profile: "retro", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		inner.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(assemble(counted, v))
	defer srv.Close()

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://" + testTeam, "aud": []string{testAUD}, "email": testMail,
		"iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
	tok.Header["kid"] = "k1"
	assertion, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}

	post := func(headers map[string]string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(nil); code != 401 {
		t.Fatalf("no credentials: status = %d, want 401", code)
	}
	if code := post(map[string]string{"Authorization": "Bearer anything"}); code != 401 {
		t.Fatalf("bearer only: status = %d, want 401", code)
	}
	if code := post(map[string]string{"Cf-Access-Jwt-Assertion": assertion + "x", "Authorization": "Bearer anything"}); code != 401 {
		t.Fatalf("bad assertion + bearer: status = %d, want 401", code)
	}
	if n := reached.Load(); n != 0 {
		t.Fatalf("MCP handler reached %d times by unauthenticated requests", n)
	}

	// With a valid assertion, a real MCP client lists no tools and cannot
	// call one that is not registered.
	client := &http.Client{Transport: headerTransport{http.DefaultTransport, map[string]string{"Cf-Access-Jwt-Assertion": assertion}}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := gmcp.NewClient(&gmcp.Implementation{Name: "e2e", Version: "0"}, nil).Connect(ctx, &gmcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: client, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if cs.InitializeResult().ServerInfo.Name != "Factortrail" {
		t.Fatalf("server name = %q", cs.InitializeResult().ServerInfo.Name)
	}
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 9 {
		t.Fatalf("tools = %d, want the 9 retro tools", len(list.Tools))
	}
	if _, err := cs.CallTool(ctx, &gmcp.CallToolParams{Name: "place_order", Arguments: map[string]any{}}); err == nil {
		t.Fatal("call of an unregistered tool succeeded")
	}
	if reached.Load() == 0 {
		t.Fatal("authenticated requests never reached the MCP handler")
	}

	// /healthz is open and carries no content; other methods are refused.
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(body) != 0 {
		t.Fatalf("healthz = %d %q", resp.StatusCode, body)
	}
	resp, _ = http.Post(srv.URL+"/healthz", "text/plain", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz = %d, want 405", resp.StatusCode)
	}
	// Anything else, authenticated or not, is a plain 404 that names nothing.
	resp, _ = http.Get(srv.URL + "/")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET / = %d, want 404", resp.StatusCode)
	}
}

// A dev server with upstream settings serves a retro tool through the whole
// stack: process entry, handler, provider and a fake upstream.
func TestServeCallsTheUpstreamWithTheTokenFromTheFile(t *testing.T) {
	const token = "synthetic-token-0123456789"
	up := fake.NewServer(t, token)
	tokenPath := filepath.Join(t.TempDir(), "retro-token")
	if err := os.WriteFile(tokenPath, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lnCh := make(chan net.Listener, 1)
	listen := func(addr string) (net.Listener, error) {
		ln, err := net.Listen("tcp", addr)
		lnCh <- ln
		return ln, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var out, errb bytes.Buffer
	env := envMap(config.EnvRetroBaseURL+"="+up.URL, config.EnvRetroTokenFile+"="+tokenPath)
	go func() {
		done <- run(ctx, []string{"serve", "--dev", "--profile", "retro", "--addr", "127.0.0.1:0"}, env, &out, &errb, listen)
	}()
	ln := <-lnCh

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"retro_positions","arguments":{"cutoff_date":"2026-01-02"}}}`
	req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(got), `"structuredContent"`) || strings.Contains(string(got), `"isError":true`) {
		t.Fatalf("status = %d body = %s", resp.StatusCode, got)
	}
	reqs := up.Requests()
	if len(reqs) != 1 || reqs[0].Token != token || reqs[0].Path != "/trading/api/retro/v1/retro_positions" {
		t.Fatalf("upstream requests = %+v", reqs)
	}
	if strings.Contains(errb.String(), token) || strings.Contains(string(got), token) {
		t.Fatal("the token appears in the logs or in the response")
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit code = %d stderr = %q", code, errb.String())
	}
}
