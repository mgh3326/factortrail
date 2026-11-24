package mcp

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/mgh3326/factortrail/internal/providers/autotrader/fake"
)

// retro_timeline sends exactly the arguments the caller gave to the one
// upstream operation, and returns the upstream body unchanged: the coverage
// object and each row's ordered events.
func TestRetroTimelineArgumentsReachTheUpstreamAndBodyIsUnchanged(t *testing.T) {
	ex := fake.Load(t)["retro_timeline"]
	s := newRetroServer(t)
	tr := s.call(t, "retro_timeline", map[string]any{
		"market": "kr", "date": "2026-01-05", "symbol": "005930", "limit": 50, "cursor": "synthetic_cursor",
	})
	if tr.IsError {
		t.Fatalf("tool error: %s", tr.text())
	}
	reqs := s.upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodGet || r.Path != "/trading/api/retro/v1/retro_timeline" || r.Token != testToken {
		t.Errorf("request = %s %s token-ok=%v, want GET retro_timeline with the token", r.Method, r.Path, r.Token == testToken)
	}
	wantQuery := map[string][]string{
		"market": {"kr"}, "date": {"2026-01-05"}, "symbol": {"005930"}, "limit": {"50"}, "cursor": {"synthetic_cursor"},
	}
	if !reflect.DeepEqual(r.Query, wantQuery) {
		t.Errorf("upstream query = %v, want %v", r.Query, wantQuery)
	}

	if got, want := compact(t, tr.StructuredContent), string(ex.Body()); got != want {
		t.Fatalf("structured content differs from the upstream body\n got: %s\nwant: %s", got, want)
	}
	var out struct {
		Coverage struct {
			ObservationLog struct {
				Status string `json:"status"`
			} `json:"observation_log"`
		} `json:"coverage"`
		Data []struct {
			Symbol string            `json:"symbol"`
			Events []json.RawMessage `json:"events"`
		} `json:"data"`
	}
	if err := json.Unmarshal(tr.StructuredContent, &out); err != nil {
		t.Fatal(err)
	}
	if out.Coverage.ObservationLog.Status != "runs_recorded" {
		t.Errorf("coverage.observation_log.status = %q, want runs_recorded", out.Coverage.ObservationLog.Status)
	}
	if len(out.Data) != 2 || len(out.Data[0].Events) != 2 || len(out.Data[1].Events) != 3 {
		t.Errorf("rows and event counts = %+v, want 2 rows with 2 and 3 events", out.Data)
	}
}

// Only market and date are required; the optional ones are then not sent.
func TestRetroTimelineOptionalArgumentsAreNotSentWhenAbsent(t *testing.T) {
	s := newRetroServer(t)
	if tr := s.call(t, "retro_timeline", map[string]any{"market": "us", "date": "2026-01-05"}); tr.IsError {
		t.Fatalf("tool error: %s", tr.text())
	}
	q := s.upstream.Requests()[0].Query
	if want := map[string][]string{"market": {"us"}, "date": {"2026-01-05"}}; !reflect.DeepEqual(q, want) {
		t.Errorf("query = %v, want %v", q, want)
	}
}

// retro_timeline refuses an out-of-contract argument before the upstream is
// called, as the other tools do.
func TestRetroTimelineRefusesInvalidArgumentsLocally(t *testing.T) {
	cases := map[string]map[string]any{
		"missing date":      {"market": "kr"},
		"missing market":    {"date": "2026-01-05"},
		"bad market":        {"market": "jp", "date": "2026-01-05"},
		"bad date":          {"market": "kr", "date": "2026-1-5"},
		"symbol with space": {"market": "kr", "date": "2026-01-05", "symbol": "005 930"},
		"limit above 500":   {"market": "kr", "date": "2026-01-05", "limit": 501},
		"unknown argument":  {"market": "kr", "date": "2026-01-05", "account": "x"},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			s := newRetroServer(t)
			tr := s.call(t, "retro_timeline", a)
			if !tr.IsError {
				t.Errorf("accepted %v", a)
			}
			if n := len(s.upstream.Requests()); n != 0 {
				t.Errorf("upstream saw %d requests for an invalid call", n)
			}
		})
	}
}
