package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/factortrail/internal/config"
)

// lockedBuffer is a writer the server goroutines and the test can share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startServe runs serve in the background on loopback ephemeral ports. It
// returns once the process has logged serve.start, so every listener it will
// open has been opened. listens counts the listen calls made so far.
func startServe(t *testing.T, getenv func(string) string, args ...string) (stop func() int, logs *lockedBuffer, listens *atomic.Int32) {
	t.Helper()
	listens = new(atomic.Int32)
	listen := func(addr string) (net.Listener, error) {
		listens.Add(1)
		return net.Listen("tcp", addr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	logs = &lockedBuffer{}
	var errb lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, append([]string{"serve"}, args...), getenv, logs, &errb, listen)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(logs.String(), `"msg":"serve.start"`) {
		select {
		case code := <-done:
			t.Fatalf("serve exited %d before starting: stderr %q", code, errb.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("serve did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop = func() int {
		cancel()
		select {
		case code := <-done:
			return code
		case <-time.After(10 * time.Second):
			t.Fatal("serve did not stop after the context was cancelled")
			return -1
		}
	}
	return stop, logs, listens
}

// servedAddr reads one bound address from the serve.start line: "addr" for
// the main listener, "metrics_addr" for the metrics listener.
func servedAddr(t *testing.T, logs *lockedBuffer, field string) string {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(logs.String()))
	for sc.Scan() {
		var line map[string]any
		if json.Unmarshal(sc.Bytes(), &line) != nil || line["msg"] != "serve.start" {
			continue
		}
		addr, _ := line[field].(string)
		if addr == "" {
			t.Fatalf("serve.start has no %s: %s", field, sc.Text())
		}
		return addr
	}
	t.Fatal("no serve.start line")
	return ""
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestMetricsListenerIsOffUnlessConfigured(t *testing.T) {
	stop, logs, listens := startServe(t, nopEnv, "--dev", "--profile", "retro", "--addr", "127.0.0.1:0")
	if n := listens.Load(); n != 1 {
		t.Fatalf("listen calls = %d, want 1 (main only)", n)
	}
	mainAddr := servedAddr(t, logs, "addr")
	if code, _ := get(t, "http://"+mainAddr+"/healthz"); code != http.StatusOK {
		t.Fatalf("healthz = %d", code)
	}
	if code, _ := get(t, "http://"+mainAddr+"/metrics"); code != http.StatusNotFound {
		t.Fatalf("main listener /metrics = %d, want 404", code)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
}

func TestMetricsListenerServesOnlyMetricsOnLoopback(t *testing.T) {
	getenv := envMap(config.EnvMetricsAddr + "=127.0.0.1:0")
	stop, logs, listens := startServe(t, getenv, "--dev", "--profile", "retro", "--addr", "127.0.0.1:0")
	if n := listens.Load(); n != 2 {
		t.Fatalf("listen calls = %d, want 2 (main and metrics)", n)
	}
	mainAddr := servedAddr(t, logs, "addr")
	metricsAddr := servedAddr(t, logs, "metrics_addr")
	if code, _ := get(t, "http://"+mainAddr+"/healthz"); code != http.StatusOK {
		t.Fatalf("healthz = %d", code)
	}
	if code, _ := get(t, "http://"+mainAddr+"/metrics"); code != http.StatusNotFound {
		t.Fatalf("main listener /metrics = %d, want 404", code)
	}
	code, body := get(t, "http://"+metricsAddr+"/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics = %d", code)
	}
	if !strings.Contains(body, `factortrail_http_requests_total{class="2xx",route="/healthz"} 1`) {
		t.Fatalf("the healthz request is not counted in:\n%s", body)
	}
	if code, _ := get(t, "http://"+metricsAddr+"/healthz"); code != http.StatusNotFound {
		t.Fatalf("metrics listener /healthz = %d, want 404", code)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
}

func TestMetricsAddrNotLoopbackIsRefusedAtBoot(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:9100", ":9100", "localhost:9100", "192.0.2.10:9100"} {
		var listens atomic.Int32
		listen := func(string) (net.Listener, error) {
			listens.Add(1)
			return nil, io.EOF
		}
		var out, errb bytes.Buffer
		code := run(context.Background(), []string{"serve", "--dev", "--profile", "retro"},
			envMap(config.EnvMetricsAddr+"="+addr), &out, &errb, listen)
		if code != 2 {
			t.Errorf("%s: exit code = %d, want 2 (stderr %q)", addr, code, errb.String())
		}
		if listens.Load() != 0 {
			t.Errorf("%s: a listener was opened before the refusal", addr)
		}
		if !strings.Contains(errb.String(), config.EnvMetricsAddr) {
			t.Errorf("%s: stderr %q does not name the setting", addr, errb.String())
		}
	}
}

func TestSentryDSNIsRefusedWithoutEchoingIt(t *testing.T) {
	const secret = "SYNTHETIC-DSN-VALUE-42"
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"serve", "--dev", "--profile", "retro"},
		envMap(config.EnvSentryDSN+"=not-a-dsn-"+secret), &out, &errb, nil)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "Sentry") {
		t.Errorf("stderr %q does not name the Sentry setting", errb.String())
	}
	if strings.Contains(errb.String(), secret) {
		t.Fatalf("stderr echoes the DSN: %q", errb.String())
	}
}

func TestLogsAreJSONAndCarryNoCredentials(t *testing.T) {
	const (
		token = "SYNTHETIC-READ-TOKEN-9d2e"
		jwt   = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJzeW50aGV0aWMifQ.c3ludGhldGljLXNpZw" // gitleaks:allow synthetic test JWT
		email = "synthetic-operator@example.test"
	)
	getenv := envMap(accessEnv...)
	stop, logs, _ := startServe(t, getenv, "--profile", "retro", "--addr", "127.0.0.1:0")
	mainAddr := servedAddr(t, logs, "addr")

	// A malformed assertion is refused before any key fetch, so the test
	// makes no network call. The token and email ride in headers that must not
	// reach the log.
	req, _ := http.NewRequest(http.MethodPost, "http://"+mainAddr+"/mcp", strings.NewReader("{}"))
	req.Header.Set("Cf-Access-Jwt-Assertion", jwt)
	req.Header.Set("X-Retro-Read-Token", token)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Cookie", "session="+token)
	req.Header.Set("X-Operator", email)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit code = %d", code)
	}

	out := logs.String()
	sawRejection := false
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(sc.Bytes(), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q", sc.Text())
		}
		if entry["msg"] == "auth.rejected" {
			sawRejection = true
		}
	}
	if !sawRejection {
		t.Fatalf("no auth.rejected line in logs:\n%s", out)
	}
	for _, secret := range []string{token, jwt, email} {
		if strings.Contains(out, secret) {
			t.Fatalf("logs contain %q:\n%s", secret, out)
		}
	}
}
