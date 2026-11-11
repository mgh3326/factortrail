package autotrader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/mgh3326/factortrail/internal/providers/autotrader/fake"
)

const testToken = "synthetic-token-0123456789"

func tokenFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func newProvider(t *testing.T, baseURL string, mod func(*Config)) *Provider {
	t.Helper()
	cfg := Config{BaseURL: baseURL, TokenFile: tokenFile(t, testToken+"\n")}
	if mod != nil {
		mod(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func date(t *testing.T, s string) openapi_types.Date {
	t.Helper()
	tm, err := time.Parse(time.DateOnly, s)
	if err != nil {
		t.Fatal(err)
	}
	return openapi_types.Date{Time: tm}
}

// opCalls maps each operation to a call built from its example's query.
func opCalls(t *testing.T) map[string]func(*Provider, map[string]string) (Result, error) {
	ctx := context.Background()
	d := func(q map[string]string, k string) openapi_types.Date { return date(t, q[k]) }
	opt := func(q map[string]string, k string) *string {
		if v, ok := q[k]; ok {
			return &v
		}
		return nil
	}
	return map[string]func(*Provider, map[string]string) (Result, error){
		"retro_day": func(p *Provider, q map[string]string) (Result, error) {
			return p.RetroDay(ctx, &RetroDayParams{Market: q["market"], Date: d(q, "date")})
		},
		"retro_symbol": func(p *Provider, q map[string]string) (Result, error) {
			market := RetroSymbolParamsMarket(q["market"])
			var marketPtr *RetroSymbolParamsMarket
			if q["market"] != "" {
				marketPtr = &market
			}
			return p.RetroSymbol(ctx, &RetroSymbolParams{Symbol: q["symbol"], Market: marketPtr, From: d(q, "from"), To: d(q, "to")})
		},
		"retro_funnel": func(p *Provider, q map[string]string) (Result, error) {
			return p.RetroFunnel(ctx, &RetroFunnelParams{Market: q["market"], From: d(q, "from"), To: d(q, "to")})
		},
		"retro_scoreboard": func(p *Provider, q map[string]string) (Result, error) {
			return p.RetroScoreboard(ctx, &RetroScoreboardParams{From: d(q, "from"), To: d(q, "to")})
		},
		"retro_positions": func(p *Provider, q map[string]string) (Result, error) {
			return p.RetroPositions(ctx, &RetroPositionsParams{CutoffDate: d(q, "cutoff_date")})
		},
		"mock_experiments": func(p *Provider, q map[string]string) (Result, error) {
			return p.MockExperiments(ctx, &MockExperimentsParams{StrategyId: opt(q, "strategy_id")})
		},
		"get_bars": func(p *Provider, q map[string]string) (Result, error) {
			market := GetBarsParamsMarket(q["market"])
			var marketPtr *GetBarsParamsMarket
			if q["market"] != "" {
				marketPtr = &market
			}
			return p.GetBars(ctx, &GetBarsParams{Symbol: q["symbol"], Market: marketPtr, Tf: GetBarsParamsTf(q["tf"]), From: d(q, "from"), To: d(q, "to")})
		},
		"data_gaps": func(p *Provider, _ map[string]string) (Result, error) {
			return p.DataGaps(ctx, &DataGapsParams{})
		},
		"retro_trades": func(p *Provider, q map[string]string) (Result, error) {
			return p.RetroTrades(ctx, &RetroTradesParams{From: d(q, "from"), To: d(q, "to"), Market: (*RetroTradesParamsMarket)(opt(q, "market")), Strategy: opt(q, "strategy")})
		},
	}
}

func canonical(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Each tool-facing operation sends one request, to its own path only, with
// the token from the file, and returns its synthetic example unchanged.
func TestEachOperationRoundTripsItsExample(t *testing.T) {
	examples := fake.Load(t)
	calls := opCalls(t)
	if len(calls) != len(fake.Operations) {
		t.Fatalf("test covers %d operations, want %d", len(calls), len(fake.Operations))
	}
	for _, op := range fake.Operations {
		t.Run(op, func(t *testing.T) {
			srv := fake.NewServer(t, testToken)
			p := newProvider(t, srv.URL, nil)
			ex := examples[op]
			got, err := calls[op](p, ex.Request.Query)
			if err != nil {
				t.Fatal(err)
			}
			if g, w := canonical(t, got), string(ex.Body()); g != w {
				t.Fatalf("result differs from the example\n got: %s\nwant: %s", g, w)
			}
			reqs := srv.Requests()
			if len(reqs) != 1 {
				t.Fatalf("upstream saw %d requests, want exactly 1", len(reqs))
			}
			r := reqs[0]
			if r.Method != http.MethodGet || r.Path != ex.Request.Path {
				t.Errorf("request = %s %s, want GET %s", r.Method, r.Path, ex.Request.Path)
			}
			if r.Token != testToken {
				t.Errorf("%s header = %q, want the token from the file", TokenHeader, r.Token)
			}
			wantQuery := map[string][]string{}
			for k, v := range ex.Request.Query {
				wantQuery[k] = []string{v}
			}
			if !reflect.DeepEqual(r.Query, wantQuery) {
				t.Errorf("query = %v, want %v", r.Query, wantQuery)
			}
		})
	}
}

func TestOptionalParametersAreSentOnlyWhenSet(t *testing.T) {
	srv := fake.NewServer(t, testToken)
	p := newProvider(t, srv.URL, nil)
	limit, cursor := 7, "synthetic_cursor"
	if _, err := p.DataGaps(context.Background(), &DataGapsParams{Limit: &limit, Cursor: &cursor}); err != nil {
		t.Fatal(err)
	}
	q := srv.Requests()[0].Query
	if !reflect.DeepEqual(q, map[string][]string{"limit": {"7"}, "cursor": {"synthetic_cursor"}}) {
		t.Fatalf("query = %v", q)
	}
}

// The token is read once, at New: later changes to the file do not reach the
// wire.
func TestTokenIsReadOnce(t *testing.T) {
	srv := fake.NewServer(t, testToken)
	path := tokenFile(t, testToken)
	p, err := New(Config{BaseURL: srv.URL, TokenFile: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("rotated-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DataGaps(context.Background(), &DataGapsParams{}); err != nil {
		t.Fatal(err)
	}
	if got := srv.Requests()[0].Token; got != testToken {
		t.Fatalf("token on the wire = %q, want the one read at start", got)
	}
}

// Nothing a call returns or a config error says contains the token, the
// token file path or an upstream body.
func TestTokenAndUpstreamBodiesNeverAppearInErrors(t *testing.T) {
	const leak = "UPSTREAM-DETAIL-ACCOUNT-123456"
	statuses := []int{401, 403, 404, 422, 500, 502, 503}
	for _, status := range statuses {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := fake.NewServer(t, testToken)
			srv.Override = func(w http.ResponseWriter, r *http.Request) {
				// Echo the token and a detail the way a careless upstream might.
				body := fmt.Sprintf(`{"code":"auth_invalid","message":"%s %s"}`, leak, r.Header.Get(fake.TokenHeader))
				fake.WriteJSON(w, status, []byte(body))
			}
			p := newProvider(t, srv.URL, nil)
			_, err := p.DataGaps(context.Background(), &DataGapsParams{})
			if err == nil {
				t.Fatal("want an error")
			}
			for _, s := range []string{err.Error(), fmt.Sprintf("%v %+v %#v", err, err, err)} {
				if strings.Contains(s, testToken) || strings.Contains(s, leak) {
					t.Fatalf("error text leaks: %q", s)
				}
			}
		})
	}
	t.Run("config errors", func(t *testing.T) {
		path := tokenFile(t, testToken+" with space")
		_, err := New(Config{BaseURL: "http://127.0.0.1:1", TokenFile: path})
		if err == nil {
			t.Fatal("want an error for a token with a space")
		}
		if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), path) {
			t.Fatalf("config error leaks: %q", err)
		}
	})
}

func codeOf(t *testing.T, err error) (ErrorCode, int) {
	t.Helper()
	var ce *CallError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v (%T) is not a *CallError", err, err)
	}
	return ce.Code, ce.Status
}

func TestUpstreamErrorsMapToContractCodes(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantCode ErrorCode
	}{
		{"401 missing", 401, `{"code":"auth_missing","message":"Authentication required."}`, ErrorCodeAuthMissing},
		{"401 invalid", 401, `{"code":"auth_invalid","message":"Authentication invalid."}`, ErrorCodeAuthInvalid},
		{"401 without a body", 401, ``, ErrorCodeAuthInvalid},
		{"401 with a code of another status", 401, `{"code":"feature_off","message":"x"}`, ErrorCodeAuthInvalid},
		{"403", 403, `{"code":"scope_denied","message":"Scope denied."}`, ErrorCodeScopeDenied},
		{"403 with an HTML body", 403, `<html>nope</html>`, ErrorCodeScopeDenied},
		{"404", 404, `{"code":"feature_off","message":"Feature unavailable."}`, ErrorCodeFeatureOff},
		{"422", 422, `{"code":"validation_error","message":"Request invalid."}`, ErrorCodeValidationError},
		{"503 timeout", 503, `{"code":"timeout","message":"Request timed out."}`, ErrorCodeTimeout},
		{"503 unavailable", 503, `{"code":"upstream_unavailable","message":"Upstream unavailable."}`, ErrorCodeUpstreamUnavailable},
		{"503 schema mismatch", 503, `{"code":"schema_mismatch","message":"Schema mismatch."}`, ErrorCodeSchemaMismatch},
		{"503 unknown code", 503, `{"code":"surprise","message":"x"}`, ErrorCodeUpstreamUnavailable},
		{"500 is not a contract status", 500, `{"code":"auth_invalid","message":"x"}`, ErrorCodeUpstreamUnavailable},
		{"204 is not a result", 204, ``, ErrorCodeUpstreamUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := fake.NewServer(t, testToken)
			srv.Override = func(w http.ResponseWriter, _ *http.Request) { fake.WriteJSON(w, tt.status, []byte(tt.body)) }
			p := newProvider(t, srv.URL, nil)
			_, err := p.RetroPositions(context.Background(), &RetroPositionsParams{CutoffDate: date(t, "2026-01-02")})
			code, status := codeOf(t, err)
			if code != tt.wantCode || status != tt.status {
				t.Fatalf("got (%s, %d), want (%s, %d)", code, status, tt.wantCode, tt.status)
			}
			if want := "autotrader " + string(tt.wantCode) + ": " + string(messages[tt.wantCode]); err.Error() != want {
				t.Fatalf("error text = %q, want %q", err, want)
			}
		})
	}
}

func TestMalformedSuccessBodyIsSchemaMismatch(t *testing.T) {
	for name, body := range map[string]string{
		"not json":      `<html>`,
		"an array":      `[]`,
		"null":          `null`,
		"a string":      `"x"`,
		"trailing data": `{"data":[]} {"data":[]}`,
		"empty":         ``,
	} {
		t.Run(name, func(t *testing.T) {
			srv := fake.NewServer(t, testToken)
			srv.Override = func(w http.ResponseWriter, _ *http.Request) { fake.WriteJSON(w, 200, []byte(body)) }
			p := newProvider(t, srv.URL, nil)
			_, err := p.DataGaps(context.Background(), &DataGapsParams{})
			if code, _ := codeOf(t, err); code != ErrorCodeSchemaMismatch {
				t.Fatalf("code = %s, want schema_mismatch", code)
			}
		})
	}
}

// A redirect is not followed: the provider reports a failure, and the
// redirect target never sees a request (and so never sees the token).
func TestRedirectIsRefused(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		fake.WriteJSON(w, 200, []byte(`{}`))
	}))
	defer target.Close()
	for _, status := range []int{301, 302, 307, 308} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := fake.NewServer(t, testToken)
			srv.Override = func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+r.URL.Path, status)
			}
			p := newProvider(t, srv.URL, nil)
			_, err := p.DataGaps(context.Background(), &DataGapsParams{})
			if code, _ := codeOf(t, err); code != ErrorCodeUpstreamUnavailable {
				t.Fatalf("code = %s, want upstream_unavailable", code)
			}
			if n := targetHits.Load(); n != 0 {
				t.Fatalf("redirect target received %d requests, want 0", n)
			}
		})
	}
}

func TestOversizeBodyIsRefused(t *testing.T) {
	srv := fake.NewServer(t, testToken)
	p := newProvider(t, srv.URL, func(c *Config) { c.MaxResponseBytes = 64 })
	_, err := p.DataGaps(context.Background(), &DataGapsParams{})
	if code, status := codeOf(t, err); code != ErrorCodeUpstreamUnavailable || status != 200 {
		t.Fatalf("got (%s, %d), want (upstream_unavailable, 200)", code, status)
	}
	// A body of exactly the cap is accepted; one byte more is not.
	body := `{"data":[]}`
	for _, tt := range []struct {
		max  int64
		fail bool
	}{{int64(len(body)), false}, {int64(len(body)) - 1, true}} {
		srv.Override = func(w http.ResponseWriter, _ *http.Request) { fake.WriteJSON(w, 200, []byte(body)) }
		p := newProvider(t, srv.URL, func(c *Config) { c.MaxResponseBytes = tt.max })
		_, err := p.DataGaps(context.Background(), &DataGapsParams{})
		if (err != nil) != tt.fail {
			t.Fatalf("cap %d: err = %v, want failure=%v", tt.max, err, tt.fail)
		}
	}
}

func TestTimeoutAndCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := fake.NewServer(t, testToken)
	srv.Override = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
	t.Cleanup(func() { close(release) })

	t.Run("per-call timeout", func(t *testing.T) {
		p := newProvider(t, srv.URL, func(c *Config) { c.Timeout = 50 * time.Millisecond })
		start := time.Now()
		_, err := p.DataGaps(context.Background(), &DataGapsParams{})
		if code, _ := codeOf(t, err); code != ErrorCodeTimeout {
			t.Fatalf("code = %s, want timeout (err %v)", code, err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("call took %v", d)
		}
	})
	t.Run("caller cancellation", func(t *testing.T) {
		p := newProvider(t, srv.URL, nil)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		_, err := p.DataGaps(ctx, &DataGapsParams{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("an already cancelled context sends nothing", func(t *testing.T) {
		srv2 := fake.NewServer(t, testToken)
		p := newProvider(t, srv2.URL, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := p.DataGaps(ctx, &DataGapsParams{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if n := len(srv2.Requests()); n != 0 {
			t.Fatalf("upstream saw %d requests", n)
		}
	})
}

// Numbers the provider sends reach the caller exactly: an integer above
// 2^53, a decimal string with trailing zeros, null, and a JSON number whose
// trailing zero would be lost by a float round trip.
func TestPrecisionIsPreserved(t *testing.T) {
	const body = `{"big":9007199254740993,"data":[{"missing":null,"price":"1.2300","quantity":"9007199254740993.0000"}],"scaled":1.50,"total":null}`
	srv := fake.NewServer(t, testToken)
	srv.Override = func(w http.ResponseWriter, _ *http.Request) { fake.WriteJSON(w, 200, []byte(body)) }
	p := newProvider(t, srv.URL, nil)
	got, err := p.DataGaps(context.Background(), &DataGapsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if got["big"] != json.Number("9007199254740993") {
		t.Errorf("big = %#v, want json.Number 9007199254740993", got["big"])
	}
	if got["scaled"] != json.Number("1.50") {
		t.Errorf("scaled = %#v, want json.Number 1.50", got["scaled"])
	}
	if v, ok := got["total"]; !ok || v != nil {
		t.Errorf("total = %#v (present %v), want present null", v, ok)
	}
	row := got["data"].([]any)[0].(map[string]any)
	if row["price"] != "1.2300" || row["quantity"] != "9007199254740993.0000" {
		t.Errorf("decimal strings changed: %#v", row)
	}
	if v, ok := row["missing"]; !ok || v != nil {
		t.Errorf("missing = %#v (present %v), want present null", v, ok)
	}
	if g := canonical(t, got); g != body {
		t.Errorf("re-encoded result differs\n got: %s\nwant: %s", g, body)
	}
}

func TestNewRefusesBadConfig(t *testing.T) {
	good := tokenFile(t, testToken)
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"no base URL", Config{TokenFile: good}, "base URL"},
		{"relative base URL", Config{BaseURL: "/trading", TokenFile: good}, "base URL"},
		{"ftp scheme", Config{BaseURL: "ftp://example.test", TokenFile: good}, "http or https"},
		{"user info", Config{BaseURL: "http://user:pw@example.test", TokenFile: good}, "user info"},
		{"query", Config{BaseURL: "http://example.test?x=1", TokenFile: good}, "query"},
		{"fragment", Config{BaseURL: "http://example.test#x", TokenFile: good}, "fragment"},
		{"no token file", Config{BaseURL: "http://example.test"}, "token file"},
		{"missing token file", Config{BaseURL: "http://example.test", TokenFile: filepath.Join(t.TempDir(), "absent")}, "cannot be read"},
		{"empty token file", Config{BaseURL: "http://example.test", TokenFile: tokenFile(t, "\n")}, "empty"},
		{"multi-line token", Config{BaseURL: "http://example.test", TokenFile: tokenFile(t, "a\nb")}, "printable ASCII"},
		{"oversize token", Config{BaseURL: "http://example.test", TokenFile: tokenFile(t, strings.Repeat("a", 5000))}, "too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "pw") && tt.name == "user info" {
				t.Fatalf("error echoes the credential: %v", err)
			}
		})
	}
}

func TestBaseURLPathPrefixAndTrailingSlash(t *testing.T) {
	srv := fake.NewServer(t, testToken)
	// The fake only serves the exact contract paths, so a prefix is checked
	// against a 404 from the right place.
	p := newProvider(t, srv.URL+"/prefix/", nil)
	_, err := p.DataGaps(context.Background(), &DataGapsParams{})
	if code, _ := codeOf(t, err); code != ErrorCodeFeatureOff {
		t.Fatalf("code = %s, want feature_off", code)
	}
	if got := srv.Paths(); !reflect.DeepEqual(got, []string{"/prefix/trading/api/retro/v1/data_gaps"}) {
		t.Fatalf("paths = %v", got)
	}
}
