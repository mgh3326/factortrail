// Package fake serves the pinned synthetic retro/v1 examples over httptest.
// It is test support for this repository's provider and MCP tests, and for a
// CI job that wants a stand-in upstream. Production code must not import it.
package fake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// TokenHeader is the header the real provider authenticates with.
const TokenHeader = "X-Retro-Read-Token"

// Example is one file of the pinned examples directory.
type Example struct {
	// File is the base name without extension, for example "retro_day".
	File      string
	Operation string `yaml:"operation"`
	Request   struct {
		Path  string            `yaml:"path"`
		Query map[string]string `yaml:"query"`
	} `yaml:"request"`
	Status   int `yaml:"status"`
	Response any `yaml:"response"`
}

// Body is the example's response as JSON.
func (e Example) Body() []byte {
	b, err := json.Marshal(e.Response)
	if err != nil {
		panic(fmt.Sprintf("example %s: %v", e.File, err))
	}
	return b
}

// ExamplesDir returns the pinned examples directory by walking up from the
// working directory to the module root.
func ExamplesDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "contracts", "autotrader", "retro", "v1", "examples"), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above the working directory")
		}
		dir = parent
	}
}

// Load reads every example, keyed by file base name.
func Load(t testing.TB) map[string]Example {
	t.Helper()
	dir, err := ExamplesDir()
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no examples in %s (%v)", dir, err)
	}
	out := make(map[string]Example, len(paths))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var e Example
		if err := yaml.Unmarshal(raw, &e); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		e.File = strings.TrimSuffix(filepath.Base(p), ".yaml")
		out[e.File] = e
	}
	return out
}

// Operations are the nine operations, in the contract's order.
var Operations = []string{
	"retro_day", "retro_symbol", "retro_funnel", "retro_scoreboard",
	"retro_positions", "mock_experiments", "get_bars", "data_gaps", "retro_trades",
}

// Request is one request the fake saw.
type Request struct {
	Method string
	Path   string
	Query  map[string][]string
	// Token is the X-Retro-Read-Token header value ("" when absent).
	Token string
}

// Server is an httptest server for the retro/v1 paths.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	requests []Request
	// Override, when set before the first request, answers instead of the
	// examples. It runs after the request is recorded.
	Override http.HandlerFunc
}

// NewServer serves, for each operation, the example whose file name equals the
// operation name, to a request that carries token. A wrong or missing token
// gets the contract's 401 body, and any other path gets 404.
func NewServer(t testing.TB, token string) *Server {
	t.Helper()
	examples := Load(t)
	byPath := map[string]Example{}
	for _, op := range Operations {
		e, ok := examples[op]
		if !ok {
			t.Fatalf("missing example %s", op)
		}
		byPath[e.Request.Path] = e
	}
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.requests = append(s.requests, Request{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Token: r.Header.Get(TokenHeader),
		})
		override := s.Override
		s.mu.Unlock()
		if override != nil {
			override(w, r)
			return
		}
		if r.Header.Get(TokenHeader) != token {
			WriteJSON(w, http.StatusUnauthorized, []byte(`{"code":"auth_invalid","message":"Authentication invalid."}`))
			return
		}
		e, ok := byPath[r.URL.Path]
		if !ok || r.Method != http.MethodGet {
			WriteJSON(w, http.StatusNotFound, []byte(`{"code":"feature_off","message":"Feature unavailable."}`))
			return
		}
		WriteJSON(w, http.StatusOK, e.Body())
	}))
	t.Cleanup(s.Close)
	return s
}

// WriteJSON writes body with the given status.
func WriteJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Requests returns a copy of the requests seen so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Paths returns the sorted distinct paths requested so far.
func (s *Server) Paths() []string {
	seen := map[string]bool{}
	for _, r := range s.Requests() {
		seen[r.Path] = true
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
