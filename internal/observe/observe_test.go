package observe_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/mgh3326/factortrail/internal/observe"
	"github.com/mgh3326/factortrail/internal/providers/autotrader"
)

// Synthetic values only. Each one must be absent from every event and every
// metric label the package produces.
const (
	synthDSN     = "https://synthetic-public-key@sentry.example.invalid/1234"
	synthToken   = "SYNTHETIC-READ-TOKEN-7f3a9c"
	synthJWT     = "eyJhbGciOiJSUzI1NiIsImtpZCI6InN5bnRoIn0.eyJlbWFpbCI6InN5bnRoZXRpY0BleGFtcGxlLnRlc3QifQ.c3ludGhldGljLXNpZw" // gitleaks:allow synthetic test JWT
	synthEmail   = "synthetic-operator@example.test"
	synthPath    = "/run/synthetic-secrets/token-file"
	synthBase    = "http://synthetic-upstream.example.invalid:8000"
	synthCookie  = "SYNTHETIC-SESSION-COOKIE-1"
	synthBearer  = "SYNTHETIC-BEARER-VALUE-2"
	synthQueryRE = "SYNTHETIC-QUERY-VALUE-3"
)

// recorder is a Sentry transport that keeps what would have been sent.
type recorder struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (r *recorder) Flush(time.Duration) bool       { return true }
func (r *recorder) Configure(sentry.ClientOptions) {}
func (r *recorder) Close()                         {}

func (r *recorder) SendEvent(ev *sentry.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}

func (r *recorder) sent() []*sentry.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*sentry.Event(nil), r.events...)
}

func newReporter(t *testing.T, rec *recorder) *observe.Errors {
	t.Helper()
	e, err := observe.NewErrors(observe.SentryOptions{
		DSN:       synthDSN,
		Release:   "test-revision",
		Redact:    []string{synthPath, synthBase, synthEmail},
		Transport: rec,
	})
	if err != nil || e == nil {
		t.Fatalf("NewErrors = %v, %v; want a reporter", e, err)
	}
	return e
}

func scrape(t *testing.T, m *observe.Metrics) string {
	t.Helper()
	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("scrape status = %d", rr.Code)
	}
	return rr.Body.String()
}

// assertClean fails when any synthetic secret or identity appears in the JSON
// of the given values.
func assertClean(t *testing.T, where string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{synthToken, synthJWT, synthEmail, synthPath, synthBase, synthCookie, synthBearer, synthQueryRE, "eyJhbGciOiJSUzI1NiIsImtpZCI6InN5bnRoIn0"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("%s contains %q", where, secret)
		}
	}
}

func TestNoDSNMeansNoClientAndNothingSent(t *testing.T) {
	// SENTRY_DSN in the environment must not switch reporting on: the SDK
	// would read it when a client is built without a DSN.
	t.Setenv("SENTRY_DSN", synthDSN)
	rec := &recorder{}
	e, err := observe.NewErrors(observe.SentryOptions{DSN: "", Transport: rec})
	if err != nil || e != nil {
		t.Fatalf("NewErrors = %v, %v; want nil, nil", e, err)
	}
	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	e.CaptureError(r, errors.New("boom"), map[string]string{"route": "/mcp"})
	e.Flush(time.Second)
	if n := len(rec.sent()); n != 0 {
		t.Fatalf("%d events sent with no DSN", n)
	}
}

func TestInvalidDSNIsRefusedWithoutEchoingIt(t *testing.T) {
	_, err := observe.NewErrors(observe.SentryOptions{DSN: "not-a-dsn-" + synthToken})
	if err == nil {
		t.Fatal("invalid DSN accepted")
	}
	if strings.Contains(err.Error(), synthToken) {
		t.Fatalf("error echoes the DSN: %v", err)
	}
}

func TestEventsLeaveWithoutCredentialsCookiesQueryOrIdentity(t *testing.T) {
	rec := &recorder{}
	e := newReporter(t, rec)

	r := httptest.NewRequest(http.MethodGet, "/mcp?token="+synthQueryRE+"&email="+synthEmail, nil)
	r.Header.Set("X-Retro-Read-Token", synthToken)
	r.Header.Set("Cf-Access-Jwt-Assertion", synthJWT)
	r.Header.Set("Cookie", "session="+synthCookie)
	r.Header.Set("Authorization", "Bearer "+synthBearer)
	r.Header.Set("X-Keep", "visible")
	// Ordinary-looking headers that carry identity or a JWT: their values are
	// not on the credential list, so they must still be scrubbed.
	r.Header.Set("X-Operator", synthEmail)
	r.Header.Set("X-Assertion", synthJWT)

	e.CaptureError(r, fmt.Errorf("reading %s for %s with %s", synthPath, synthEmail, synthJWT), map[string]string{"route": "/mcp"})
	e.Flush(time.Second)

	events := rec.sent()
	if len(events) != 1 {
		t.Fatalf("sent %d events, want 1", len(events))
	}
	ev := events[0]
	assertClean(t, "event", ev)
	if ev.Request == nil {
		t.Fatal("event has no request shape; the scrubbing check would prove nothing")
	}
	if _, ok := ev.Request.Headers["X-Keep"]; !ok {
		t.Errorf("non-sensitive header dropped: %v", ev.Request.Headers)
	}
	if !strings.Contains(ev.Message+ev.Exception[0].Value, "[redacted]") {
		t.Errorf("nothing was redacted in %q", ev.Exception[0].Value)
	}
	if ev.Release != "test-revision" || ev.Environment != "" {
		t.Errorf("release/environment = %q/%q", ev.Release, ev.Environment)
	}
}

func TestPanicIsAnswered500AndReportedWithoutSecrets(t *testing.T) {
	rec := &recorder{}
	e := newReporter(t, rec)
	m := observe.NewMetrics()
	h := observe.Instrument(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("upstream refused " + synthJWT + " at " + synthPath)
	}), m, e)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp?x="+synthQueryRE, nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if body := rr.Body.String(); strings.Contains(body, synthJWT) || strings.Contains(body, synthPath) {
		t.Fatalf("panic detail reached the client: %q", body)
	}
	e.Flush(time.Second)
	events := rec.sent()
	if len(events) != 1 {
		t.Fatalf("sent %d events, want 1", len(events))
	}
	assertClean(t, "panic event", events[0])
	if !strings.Contains(scrape(t, m), `factortrail_http_requests_total{class="5xx",route="/mcp"} 1`) {
		t.Error("the 500 is not counted on its route")
	}
}

func TestServerErrorIsReportedOnceWithItsFixedRoute(t *testing.T) {
	rec := &recorder{}
	e := newReporter(t, rec)
	h := observe.Instrument(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}), observe.NewMetrics(), e)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/some/"+synthToken, nil))
	e.Flush(time.Second)
	events := rec.sent()
	if len(events) != 1 {
		t.Fatalf("sent %d events, want 1", len(events))
	}
	assertClean(t, "5xx event", events[0])
	if got := events[0].Tags["route"]; got != "other" {
		t.Fatalf("route tag = %q, want the fixed label %q", got, "other")
	}
}

func TestMetricLabelsStayFixed(t *testing.T) {
	m := observe.NewMetrics()
	h := observe.Instrument(http.NotFoundHandler(), m, nil)
	for _, path := range []string{"/mcp", "/healthz", "/tok-" + synthToken + "/" + synthEmail, "/" + synthPath} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}
	m.ObserveTool("retro_day", 3*time.Millisecond, false)
	m.ObserveTool("retro_day", 3*time.Millisecond, true)
	m.ObserveTool(synthToken, time.Millisecond, true)

	body := scrape(t, m)
	assertClean(t, "metrics output", body)
	for _, want := range []string{
		`factortrail_http_requests_total{class="4xx",route="/mcp"} 1`,
		`factortrail_http_requests_total{class="4xx",route="/healthz"} 1`,
		`factortrail_http_requests_total{class="4xx",route="other"} 2`,
		`factortrail_mcp_tool_calls_total{result="ok",tool="retro_day"} 1`,
		`factortrail_mcp_tool_calls_total{result="error",tool="retro_day"} 1`,
		`factortrail_mcp_tool_calls_total{result="error",tool="other"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output lacks %s", want)
		}
	}
}

type fakeUpstream struct{ err error }

func (f fakeUpstream) RetroDay(context.Context, *autotrader.RetroDayParams) (autotrader.Result, error) {
	return nil, f.err
}
func (f fakeUpstream) RetroSymbol(context.Context, *autotrader.RetroSymbolParams) (autotrader.Result, error) {
	return nil, f.err
}
func (f fakeUpstream) RetroFunnel(context.Context, *autotrader.RetroFunnelParams) (autotrader.Result, error) {
	return nil, f.err
}
func (f fakeUpstream) RetroScoreboard(context.Context, *autotrader.RetroScoreboardParams) (autotrader.Result, error) {
	return nil, f.err
}
func (f fakeUpstream) RetroPositions(context.Context, *autotrader.RetroPositionsParams) (autotrader.Result, error) {
	return nil, f.err
}
func (f fakeUpstream) MockExperiments(context.Context, *autotrader.MockExperimentsParams) (autotrader.Result, error) {
	return nil, f.err
}
func (f fakeUpstream) GetBars(context.Context, *autotrader.GetBarsParams) (autotrader.Result, error) {
	return nil, f.err
}
func (f fakeUpstream) DataGaps(context.Context, *autotrader.DataGapsParams) (autotrader.Result, error) {
	return nil, f.err
}

func TestUpstreamFailuresAreCountedAndOnlyOperatorFaultsReported(t *testing.T) {
	rec := &recorder{}
	e := newReporter(t, rec)
	m := observe.NewMetrics()

	outage := &autotrader.CallError{Code: autotrader.ErrorCodeUpstreamUnavailable, Status: 503}
	u := observe.NewObservedUpstream(fakeUpstream{err: outage}, m, e)
	_, err := u.RetroDay(context.Background(), &autotrader.RetroDayParams{})
	if !errors.Is(err, outage) {
		t.Fatalf("error changed: %v", err)
	}

	// A validation answer is counted but is not an operator fault.
	u = observe.NewObservedUpstream(fakeUpstream{err: &autotrader.CallError{Code: autotrader.ErrorCodeValidationError, Status: 422}}, m, e)
	_, _ = u.MockExperiments(context.Background(), &autotrader.MockExperimentsParams{})

	u = observe.NewObservedUpstream(fakeUpstream{}, m, e)
	if _, err := u.GetBars(context.Background(), &autotrader.GetBarsParams{}); err != nil {
		t.Fatalf("success path returned %v", err)
	}
	e.Flush(time.Second)

	events := rec.sent()
	if len(events) != 1 {
		t.Fatalf("sent %d events, want 1 (the outage only)", len(events))
	}
	assertClean(t, "upstream event", events[0])
	if events[0].Tags["operation"] != "retro_day" || events[0].Tags["outcome"] != "upstream_unavailable" {
		t.Fatalf("tags = %v", events[0].Tags)
	}

	body := scrape(t, m)
	for _, want := range []string{
		`factortrail_upstream_requests_total{operation="retro_day",outcome="upstream_unavailable"} 1`,
		`factortrail_upstream_requests_total{operation="mock_experiments",outcome="validation_error"} 1`,
		`factortrail_upstream_requests_total{operation="get_bars",outcome="ok"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output lacks %s", want)
		}
	}
}
