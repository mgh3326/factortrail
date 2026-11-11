package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// connect joins a client to srv over an in-memory pipe.
func connect(t *testing.T, srv *gmcp.Server) *gmcp.ClientSession {
	t.Helper()
	ctx := ctxT(t)
	ct, st := gmcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := gmcp.NewClient(&gmcp.Implementation{Name: "test-client", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func listNames(t *testing.T, cs *gmcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(ctxT(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

type echoIn struct {
	Text string `json:"text"`
}

// testTool returns a table entry whose handler counts its calls.
func testTool(name string, calls *atomic.Int32) tool {
	return tool{name: name, register: func(s *gmcp.Server, _ Retro) {
		gmcp.AddTool(s, &gmcp.Tool{Name: name, Description: "test tool"}, func(_ context.Context, _ *gmcp.CallToolRequest, in echoIn) (*gmcp.CallToolResult, any, error) {
			calls.Add(1)
			return &gmcp.CallToolResult{Content: []gmcp.Content{&gmcp.TextContent{Text: in.Text}}}, nil, nil
		})
	}}
}

func TestUnregisteredToolIsRefused(t *testing.T) {
	p, _ := lookupProfile("retro")
	cs := connect(t, newServer("test", p, nil, nil))
	res, err := cs.CallTool(ctxT(t), &gmcp.CallToolParams{Name: "place_order", Arguments: map[string]any{}})
	if err == nil {
		t.Fatalf("call of an unregistered tool succeeded: %+v", res)
	}
	if !strings.Contains(err.Error(), "place_order") {
		t.Fatalf("error %q does not name the refused tool", err)
	}
}

func TestProfileToolsAreServedAndOthersAreNot(t *testing.T) {
	var inCalls, strayCalls atomic.Int32
	srv := newServer("test", []tool{testTool("in_profile", &inCalls)}, nil, nil)

	// A handler that exists on the server but is not in the profile table,
	// as if a later change registered it by mistake.
	strayTool := testTool("stray", &strayCalls)
	strayTool.register(srv, nil)

	cs := connect(t, srv)
	if got := listNames(t, cs); len(got) != 1 || got[0] != "in_profile" {
		t.Fatalf("tools/list = %v, want only [in_profile]", got)
	}
	res, err := cs.CallTool(ctxT(t), &gmcp.CallToolParams{Name: "in_profile", Arguments: map[string]any{"text": "hi"}})
	if err != nil || inCalls.Load() != 1 {
		t.Fatalf("in-profile call err = %v calls = %d", err, inCalls.Load())
	}
	if tc, ok := res.Content[0].(*gmcp.TextContent); !ok || tc.Text != "hi" {
		t.Fatalf("unexpected result %+v", res.Content)
	}
	if _, err := cs.CallTool(ctxT(t), &gmcp.CallToolParams{Name: "stray", Arguments: map[string]any{"text": "x"}}); err == nil {
		t.Fatal("call of a tool outside the profile succeeded although a handler exists")
	}
	if strayCalls.Load() != 0 {
		t.Fatalf("handler outside the profile ran %d times", strayCalls.Load())
	}
}

func TestLookupProfileRefusesEmptyAndUnknown(t *testing.T) {
	for _, name := range []string{"", " ", "RETRO", "retro ", "all", "default"} {
		if _, err := lookupProfile(name); err == nil {
			t.Errorf("profile %q accepted", name)
		}
	}
	if _, err := NewHandler(Options{Profile: ""}); err == nil {
		t.Error("NewHandler accepted an empty profile")
	}
	if _, err := NewHandler(Options{Profile: "nope"}); err == nil || !strings.Contains(err.Error(), "retro") {
		t.Errorf("NewHandler error for an unknown profile = %v, want it to list the known profiles", err)
	}
}

func rawPost(t *testing.T, url, host string, headers map[string]string, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if host != "" {
		req.Host = host
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

const listBody = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

func TestToolsListOverHTTPIsStatelessAndListsTheProfile(t *testing.T) {
	h, err := NewHandler(Options{Profile: "retro", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, body := rawPost(t, srv.URL, "", nil, listBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.StatusCode, body)
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.Fatalf("stateless server set Mcp-Session-Id %q", sid)
	}
	var out struct {
		Result struct {
			Tools json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	var tools []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out.Result.Tools, &tools); err != nil || len(tools) != 9 {
		t.Fatalf("tools = %s (%v), want the nine retro tools", out.Result.Tools, err)
	}
	// A call to a tool that is not registered is a JSON-RPC error, not a result.
	_, body = rawPost(t, srv.URL, "", nil, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"place_order","arguments":{}}}`)
	if !strings.Contains(body, `"error"`) || strings.Contains(body, `"result"`) {
		t.Fatalf("unregistered tool call body = %s, want a JSON-RPC error", body)
	}
}

// Host protection. The SDK's guard rejects a loopback request carrying a
// non-loopback Host. It stays on unless a trusted public host is configured,
// and then only that exact host is admitted.
func TestHostProtection(t *testing.T) {
	const public = "tools.example.com"
	tests := []struct {
		name       string
		publicHost string
		host       string
		want       int
	}{
		{"no public host: loopback Host passes", "", "", http.StatusOK},
		{"no public host: foreign Host refused by the SDK guard", "", public, http.StatusForbidden},
		{"public host: exact match passes", public, public, http.StatusOK},
		{"public host: case-insensitive match passes", public, "Tools.Example.COM", http.StatusOK},
		{"public host: loopback Host refused", public, "", http.StatusForbidden},
		{"public host: suffix refused", public, public + ".evil.example", http.StatusForbidden},
		{"public host: prefix refused", public, "x" + public, http.StatusForbidden},
		{"public host: port refused", public, public + ":8443", http.StatusForbidden},
		{"public host: other host refused", public, "other.example.com", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := NewHandler(Options{Profile: "retro", Version: "test", PublicHost: tt.publicHost})
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(h) // listens on 127.0.0.1
			defer srv.Close()
			resp, body := rawPost(t, srv.URL, tt.host, nil, listBody)
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, tt.want, body)
			}
		})
	}
}

func TestCrossOriginRequestsAreRefused(t *testing.T) {
	h, err := NewHandler(Options{Profile: "retro", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	tests := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no browser headers (non-browser client)", nil, http.StatusOK},
		{"same-origin fetch", map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"cross-site fetch", map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"same-site fetch", map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"foreign Origin", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := rawPost(t, srv.URL, "", tt.headers, listBody)
			if resp.StatusCode != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, tt.want, body)
			}
		})
	}
}

func TestRequestBodyIsBounded(t *testing.T) {
	h, err := NewHandler(Options{Profile: "retro", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	big := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x","arguments":{"a":"` + strings.Repeat("a", 2<<20) + `"}}}`
	resp, _ := rawPost(t, srv.URL, "", nil, big)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("2 MiB request body accepted")
	}
}

// The Host match is exact over ASCII. Unicode case folding would let the
// Kelvin sign (U+212A) stand in for "k".
func TestExactHostDoesNotUnicodeFold(t *testing.T) {
	reached := false
	h := exactHost("kit.example.com", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	for host, want := range map[string]bool{
		"kit.example.com":         true,
		"KIT.Example.COM":         true,
		"Kit.example.com":         false,
		"kit.example.com.":        false,
		"kit.example.comK":        false,
		"":                        false,
		"kit.example.com, x.test": false,
	} {
		reached = false
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Host = host
		h.ServeHTTP(httptest.NewRecorder(), r)
		if reached != want {
			t.Errorf("Host %q: reached = %v, want %v", host, reached, want)
		}
	}
}
