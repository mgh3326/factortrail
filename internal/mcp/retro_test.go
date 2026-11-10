package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	"github.com/mgh3326/factortrail/internal/providers/autotrader"
	"github.com/mgh3326/factortrail/internal/providers/autotrader/fake"
)

const testToken = "synthetic-token-0123456789"

// retroServer is the whole stack under test: the MCP HTTP handler, the real
// provider and an httptest upstream that serves the pinned examples.
type retroServer struct {
	mcp      *httptest.Server
	upstream *fake.Server
	logs     *bytes.Buffer
}

func newRetroServer(t *testing.T) *retroServer {
	t.Helper()
	up := fake.NewServer(t, testToken)
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prov, err := autotrader.New(autotrader.Config{BaseURL: up.URL, TokenFile: tokenPath})
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	h, err := NewHandler(Options{
		Profile: "retro", Version: "test", Retro: prov,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &retroServer{mcp: srv, upstream: up, logs: logs}
}

// rpc posts one JSON-RPC request and returns the raw "result" and "error".
func (s *retroServer) rpc(t *testing.T, method string, params any) (result, rpcErr json.RawMessage) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	resp, text := rawPost(t, s.mcp.URL, "", nil, string(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status %d body %s", method, resp.StatusCode, text)
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	return out.Result, out.Error
}

type toolResult struct {
	IsError           bool            `json:"isError"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	Content           []struct {
		Text string `json:"text"`
	} `json:"content"`
}

func (s *retroServer) call(t *testing.T, name string, args map[string]any) toolResult {
	t.Helper()
	result, rpcErr := s.rpc(t, "tools/call", map[string]any{"name": name, "arguments": args})
	if rpcErr != nil {
		t.Fatalf("%s: JSON-RPC error %s", name, rpcErr)
	}
	var tr toolResult
	if err := json.Unmarshal(result, &tr); err != nil {
		t.Fatal(err)
	}
	return tr
}

func (tr toolResult) text() string {
	var parts []string
	for _, c := range tr.Content {
		parts = append(parts, c.Text)
	}
	return strings.Join(parts, "\n")
}

func compact(t *testing.T, raw []byte) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatalf("compact %q: %v", raw, err)
	}
	return b.String()
}

func args(q map[string]string) map[string]any {
	out := make(map[string]any, len(q))
	for k, v := range q {
		out[k] = v
	}
	return out
}

func TestRetroProfileIsExactlyTheEightContractTools(t *testing.T) {
	p, err := lookupProfile("retro")
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, tl := range p {
		rows = append(rows, tl.name)
	}
	want := append([]string(nil), fake.Operations...)
	sort.Strings(want)
	sort.Strings(rows)
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("profile rows = %v, want %v", rows, want)
	}

	// tools/list over the real handler, with the real provider behind it.
	s := newRetroServer(t)
	result, _ := s.rpc(t, "tools/list", map[string]any{})
	var list struct {
		Tools []struct {
			Name         string                `json:"name"`
			Annotations  *gmcp.ToolAnnotations `json:"annotations"`
			InputSchema  json.RawMessage       `json:"inputSchema"`
			OutputSchema json.RawMessage       `json:"outputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &list); err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, tl := range list.Tools {
		listed = append(listed, tl.Name)
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s is not annotated read-only", tl.Name)
		}
	}
	sort.Strings(listed)
	if !reflect.DeepEqual(listed, want) {
		t.Fatalf("tools/list = %v, want exactly %v", listed, want)
	}
}

// Each tool's input schema equals the query parameters of its operation in
// the pinned spec: same names, same required set, same types and limits.
func TestToolInputsEqualTheContractQueryParameters(t *testing.T) {
	dir, err := fake.ExamplesDir()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "..", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	type param struct {
		Name     string `yaml:"name"`
		In       string `yaml:"in"`
		Required bool   `yaml:"required"`
		Schema   struct {
			Type      string   `yaml:"type"`
			Format    string   `yaml:"format"`
			Minimum   *float64 `yaml:"minimum"`
			Maximum   *float64 `yaml:"maximum"`
			MaxLength *int     `yaml:"maxLength"`
			Pattern   string   `yaml:"pattern"`
			Enum      []string `yaml:"enum"`
		} `yaml:"schema"`
	}
	var spec struct {
		Paths map[string]struct {
			Get struct {
				OperationID string  `yaml:"operationId"`
				Parameters  []param `yaml:"parameters"`
			} `yaml:"get"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	ops := map[string][]param{}
	for _, p := range spec.Paths {
		ops[p.Get.OperationID] = p.Get.Parameters
	}

	s := newRetroServer(t)
	result, _ := s.rpc(t, "tools/list", map[string]any{})
	var list struct {
		Tools []struct {
			Name        string             `json:"name"`
			InputSchema *jsonschema.Schema `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &list); err != nil {
		t.Fatal(err)
	}
	for _, tl := range list.Tools {
		t.Run(tl.Name, func(t *testing.T) {
			params := ops[tl.Name]
			if len(params) == 0 {
				t.Fatalf("no parameters for %s in the pinned spec", tl.Name)
			}
			in := tl.InputSchema
			if in.Type != "object" || in.AdditionalProperties == nil || in.AdditionalProperties.Not == nil {
				t.Errorf("input schema is not a closed object: %+v", in)
			}
			var wantRequired []string
			names := map[string]bool{}
			for _, p := range params {
				if p.In != "query" {
					t.Errorf("parameter %s is %q, not query", p.Name, p.In)
				}
				names[p.Name] = true
				if p.Required {
					wantRequired = append(wantRequired, p.Name)
				}
				prop := in.Properties[p.Name]
				if prop == nil {
					t.Errorf("no input property %s", p.Name)
					continue
				}
				if prop.Type != p.Schema.Type || prop.Format != p.Schema.Format {
					t.Errorf("%s: type/format = %s/%s, spec %s/%s", p.Name, prop.Type, prop.Format, p.Schema.Type, p.Schema.Format)
				}
				if !reflect.DeepEqual(prop.Minimum, p.Schema.Minimum) || !reflect.DeepEqual(prop.Maximum, p.Schema.Maximum) {
					t.Errorf("%s: bounds differ from the spec", p.Name)
				}
				if !reflect.DeepEqual(prop.MaxLength, p.Schema.MaxLength) {
					t.Errorf("%s: maxLength differs from the spec", p.Name)
				}
				if p.Schema.Format != "date" && prop.Pattern != p.Schema.Pattern {
					t.Errorf("%s: pattern %q, spec %q", p.Name, prop.Pattern, p.Schema.Pattern)
				}
				var gotEnum []string
				for _, e := range prop.Enum {
					gotEnum = append(gotEnum, fmt.Sprint(e))
				}
				if !reflect.DeepEqual(gotEnum, p.Schema.Enum) {
					t.Errorf("%s: enum %v, spec %v", p.Name, gotEnum, p.Schema.Enum)
				}
			}
			for name := range in.Properties {
				if !names[name] {
					t.Errorf("input property %s is not a query parameter of the operation", name)
				}
			}
			sort.Strings(wantRequired)
			gotRequired := append([]string(nil), in.Required...)
			sort.Strings(gotRequired)
			if len(wantRequired) != 0 && !reflect.DeepEqual(gotRequired, wantRequired) {
				t.Errorf("required = %v, spec %v", gotRequired, wantRequired)
			}
			if len(wantRequired) == 0 && len(gotRequired) != 0 {
				t.Errorf("required = %v, spec has none", gotRequired)
			}
		})
	}
}

// Every tool returns its synthetic example as structured content, unchanged,
// after exactly one upstream request to its own path with the file's token.
func TestEachToolRoundTripsItsExample(t *testing.T) {
	examples := fake.Load(t)
	for _, op := range fake.Operations {
		t.Run(op, func(t *testing.T) {
			s := newRetroServer(t)
			ex := examples[op]
			tr := s.call(t, op, args(ex.Request.Query))
			if tr.IsError {
				t.Fatalf("tool error: %s", tr.text())
			}
			if got, want := compact(t, tr.StructuredContent), string(ex.Body()); got != want {
				t.Fatalf("structured content differs from the example\n got: %s\nwant: %s", got, want)
			}
			reqs := s.upstream.Requests()
			if len(reqs) != 1 {
				t.Fatalf("upstream saw %d requests, want 1", len(reqs))
			}
			r := reqs[0]
			if r.Method != http.MethodGet || r.Path != ex.Request.Path || r.Token != testToken {
				t.Errorf("request = %s %s token-ok=%v, want GET %s with the token", r.Method, r.Path, r.Token == testToken, ex.Request.Path)
			}
			wantQuery := map[string][]string{}
			for k, v := range ex.Request.Query {
				wantQuery[k] = []string{v}
			}
			if !reflect.DeepEqual(r.Query, wantQuery) {
				t.Errorf("upstream query = %v, want %v", r.Query, wantQuery)
			}
		})
	}
}

func TestPaginationArgumentsReachTheUpstream(t *testing.T) {
	s := newRetroServer(t)
	tr := s.call(t, "data_gaps", map[string]any{"limit": 500, "cursor": "synthetic_cursor"})
	if tr.IsError {
		t.Fatal(tr.text())
	}
	q := s.upstream.Requests()[0].Query
	if !reflect.DeepEqual(q, map[string][]string{"limit": {"500"}, "cursor": {"synthetic_cursor"}}) {
		t.Fatalf("query = %v", q)
	}
}

// An integer above 2^53, a decimal string with trailing zeros and null reach
// the MCP result byte for byte.
func TestPrecisionReachesTheToolResult(t *testing.T) {
	const upstreamBody = `{"as_of":"2026-01-03T00:00:00Z","big":9007199254740993,"data":[{"price":"1.2300","quantity":"9007199254740993.0000","reason":null}],"scaled":1.50,"total":null}`
	s := newRetroServer(t)
	s.upstream.Override = func(w http.ResponseWriter, _ *http.Request) { fake.WriteJSON(w, 200, []byte(upstreamBody)) }
	tr := s.call(t, "data_gaps", nil)
	if tr.IsError {
		t.Fatal(tr.text())
	}
	if got := compact(t, tr.StructuredContent); got != upstreamBody {
		t.Fatalf("structured content changed\n got: %s\nwant: %s", got, upstreamBody)
	}
	for _, want := range []string{`"big":9007199254740993`, `"price":"1.2300"`, `"quantity":"9007199254740993.0000"`, `"reason":null`, `"scaled":1.50`} {
		if !strings.Contains(string(tr.StructuredContent), want) {
			t.Errorf("structured content lacks %s: %s", want, tr.StructuredContent)
		}
		if !strings.Contains(tr.text(), want) {
			t.Errorf("text content lacks %s: %s", want, tr.text())
		}
	}
}

// An upstream failure is a tool error with the contract's fixed message. The
// upstream body and the token appear nowhere: not in the result, not in logs.
func TestUpstreamFailuresAreSafeToolErrors(t *testing.T) {
	const leak = "UPSTREAM-DETAIL-ACCOUNT-123456"
	tests := []struct {
		status int
		code   string
		want   string
	}{
		{401, "auth_invalid", "Authentication invalid."},
		{403, "scope_denied", "Scope denied."},
		{404, "feature_off", "Feature unavailable."},
		{422, "validation_error", "Request invalid."},
		{503, "upstream_unavailable", "Upstream unavailable."},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			s := newRetroServer(t)
			s.upstream.Override = func(w http.ResponseWriter, r *http.Request) {
				fake.WriteJSON(w, tt.status, []byte(fmt.Sprintf(`{"code":%q,"message":"%s %s"}`, tt.code, leak, r.Header.Get(fake.TokenHeader))))
			}
			tr := s.call(t, "retro_positions", map[string]any{"cutoff_date": "2026-01-02"})
			if !tr.IsError || tr.StructuredContent != nil {
				t.Fatalf("result = %+v, want a tool error without structured content", tr)
			}
			if !strings.Contains(tr.text(), tt.code) || !strings.Contains(tr.text(), tt.want) {
				t.Errorf("error text %q lacks %s / %s", tr.text(), tt.code, tt.want)
			}
			for _, out := range []string{tr.text(), s.logs.String()} {
				if strings.Contains(out, testToken) || strings.Contains(out, leak) {
					t.Fatalf("token or upstream detail leaked: %q", out)
				}
			}
		})
	}
}

// Arguments outside the contract are refused before any upstream request, so
// a caller cannot pick a URL, path or operation.
func TestInvalidArgumentsNeverReachTheUpstream(t *testing.T) {
	tests := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"url argument", "data_gaps", map[string]any{"url": "http://evil.example/"}},
		{"path argument", "retro_day", map[string]any{"market": "XSYN", "date": "2026-01-02", "path": "/admin"}},
		{"operation argument", "retro_day", map[string]any{"market": "XSYN", "date": "2026-01-02", "operation": "get_bars"}},
		{"base_url argument", "get_bars", map[string]any{"symbol": "S", "tf": "1d", "from": "2026-01-02", "to": "2026-01-02", "base_url": "http://evil.example"}},
		{"missing required", "retro_day", map[string]any{"market": "XSYN"}},
		{"malformed date", "retro_day", map[string]any{"market": "XSYN", "date": "2026-13-45"}},
		{"date with time", "retro_day", map[string]any{"market": "XSYN", "date": "2026-01-02T00:00:00Z"}},
		{"limit zero", "data_gaps", map[string]any{"limit": 0}},
		{"limit above 500", "data_gaps", map[string]any{"limit": 501}},
		{"limit not an integer", "data_gaps", map[string]any{"limit": 1.5}},
		{"cursor with a slash", "data_gaps", map[string]any{"cursor": "a/b"}},
		{"cursor too long", "data_gaps", map[string]any{"cursor": strings.Repeat("a", 129)}},
		{"timeframe outside the enum", "get_bars", map[string]any{"symbol": "S", "tf": "2h", "from": "2026-01-02", "to": "2026-01-02"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newRetroServer(t)
			tr := s.call(t, tt.tool, tt.args)
			if !tr.IsError {
				t.Fatalf("accepted: %s", tr.StructuredContent)
			}
			if n := len(s.upstream.Requests()); n != 0 {
				t.Fatalf("upstream saw %d requests, want 0", n)
			}
		})
	}
}

// A name outside the profile is neither listed nor callable, and a call
// reaches no upstream.
func TestUnregisteredToolsAreRefusedWithoutAnUpstreamCall(t *testing.T) {
	s := newRetroServer(t)
	for _, name := range []string{"place_order", "retro_day ", "RETRO_DAY", "retro_order", "", "../retro_day"} {
		_, rpcErr := s.rpc(t, "tools/call", map[string]any{"name": name, "arguments": map[string]any{}})
		if rpcErr == nil {
			t.Errorf("call of %q did not fail", name)
		}
	}
	if n := len(s.upstream.Requests()); n != 0 {
		t.Fatalf("upstream saw %d requests, want 0", n)
	}
}

// Without upstream settings the tools stay listed, and a call says so.
func TestToolsAnswerWhenTheUpstreamIsNotConfigured(t *testing.T) {
	cs := connect(t, newServer("test", retroTools, nil))
	if got := listNames(t, cs); len(got) != 8 {
		t.Fatalf("tools/list = %v, want 8 tools", got)
	}
	res, err := cs.CallTool(ctxT(t), &gmcp.CallToolParams{Name: "data_gaps", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(fmt.Sprint(res.Content[0].(*gmcp.TextContent).Text), "not configured") {
		t.Fatalf("result = %+v, want a tool error saying the upstream is not configured", res)
	}
}
