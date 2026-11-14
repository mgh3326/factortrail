package mcp

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/mgh3326/factortrail/internal/providers/autotrader/fake"
)

// rowsOf decodes the data array of a structured tool result.
func rowsOf(t *testing.T, raw json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var env struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	return env.Data
}

// rowWithSymbol returns the row whose symbol is sym.
func rowWithSymbol(t *testing.T, rows []map[string]json.RawMessage, sym string) map[string]json.RawMessage {
	t.Helper()
	for _, r := range rows {
		var s string
		if err := json.Unmarshal(r["symbol"], &s); err == nil && s == sym {
			return r
		}
	}
	t.Fatalf("no row with symbol %s", sym)
	return nil
}

// The L7 optional fields reach the caller unchanged when the provider sends
// them: the values, and the exact decimal strings, are not reshaped.
func TestL7FieldsPassThroughWhenPresent(t *testing.T) {
	examples := fake.Load(t)

	t.Run("retro_day link fields", func(t *testing.T) {
		s := newRetroServer(t)
		ex := examples["retro_day"]
		tr := s.call(t, "retro_day", args(ex.Request.Query))
		if tr.IsError {
			t.Fatalf("tool error: %s", tr.text())
		}
		row := rowWithSymbol(t, rowsOf(t, tr.StructuredContent), "SYN003")
		for key, want := range map[string]string{
			"link_basis":          `"recorded"`,
			"decision_ref":        `"ref_synthetic_decision_3"`,
			"decision_link_basis": `"recorded"`,
		} {
			if got := string(row[key]); got != want {
				t.Errorf("%s = %s, want %s", key, got, want)
			}
		}
	})

	t.Run("retro_trades decision, fill time and costs", func(t *testing.T) {
		s := newRetroServer(t)
		ex := examples["retro_trades"]
		tr := s.call(t, "retro_trades", args(ex.Request.Query))
		if tr.IsError {
			t.Fatalf("tool error: %s", tr.text())
		}
		rows := rowsOf(t, tr.StructuredContent)
		if len(rows) == 0 {
			t.Fatal("no trade rows in the example")
		}
		row := rows[0]
		if got, want := string(row["decision_ref"]), `"decision-2cdcff0b8bcfc0576887"`; got != want {
			t.Errorf("decision_ref = %s, want %s", got, want)
		}
		if got, want := string(row["fill_time_basis"]), `"broker_order_time"`; got != want {
			t.Errorf("fill_time_basis = %s, want %s", got, want)
		}
		var costs struct {
			Recorded struct {
				Fee            json.Number `json:"fee"`
				Tax            json.Number `json:"tax"`
				Currency       string      `json:"currency"`
				FeeIncludesTax bool        `json:"fee_includes_tax"`
			} `json:"recorded"`
			Estimated struct {
				Amount json.Number `json:"amount"`
				Basis  string      `json:"basis"`
			} `json:"estimated"`
		}
		if err := json.Unmarshal(row["costs"], &costs); err != nil {
			t.Fatalf("costs: %v", err)
		}
		if costs.Recorded.Fee != "12" || costs.Recorded.Tax != "3" || costs.Recorded.Currency != "KRW" || costs.Recorded.FeeIncludesTax {
			t.Errorf("recorded costs = %+v", costs.Recorded)
		}
		if costs.Estimated.Amount != "1.05" || costs.Estimated.Basis != "inference" {
			t.Errorf("estimated costs = %+v", costs.Estimated)
		}
	})
}

// The same fields are absent, not null, when the provider omits them: the
// tool neither invents nor reshapes a value. The upstream body is the trades
// example with every L7 key removed.
func TestL7FieldsAreAbsentWhenTheProviderOmitsThem(t *testing.T) {
	examples := fake.Load(t)
	ex := examples["retro_trades"]
	var body map[string]any
	if err := json.Unmarshal(ex.Body(), &body); err != nil {
		t.Fatal(err)
	}
	rows, ok := body["data"].([]any)
	if !ok || len(rows) == 0 {
		t.Fatal("trades example has no data rows")
	}
	l7 := []string{"decision_ref", "decision_link_basis", "link_reason", "fill_time_basis", "costs"}
	for _, r := range rows {
		m := r.(map[string]any)
		for _, k := range l7 {
			delete(m, k)
		}
	}
	stripped, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	s := newRetroServer(t)
	s.upstream.Override = func(w http.ResponseWriter, _ *http.Request) {
		fake.WriteJSON(w, http.StatusOK, stripped)
	}
	tr := s.call(t, "retro_trades", args(ex.Request.Query))
	if tr.IsError {
		t.Fatalf("tool error: %s", tr.text())
	}
	for i, row := range rowsOf(t, tr.StructuredContent) {
		for _, k := range l7 {
			if _, present := row[k]; present {
				t.Errorf("row %d: %s present although the provider omitted it", i, k)
			}
		}
	}
}

// retro_decisions maps 1:1 to its operation: each argument reaches the
// matching query parameter, and an omitted optional one is not sent.
func TestRetroDecisionsArgumentsReachTheUpstream(t *testing.T) {
	s := newRetroServer(t)
	tr := s.call(t, "retro_decisions", map[string]any{
		"from": "2026-01-01", "to": "2026-01-31", "market": "us",
		"symbol": "SYN001", "limit": 50, "cursor": "synthetic_cursor",
	})
	if tr.IsError {
		t.Fatalf("tool error: %s", tr.text())
	}
	reqs := s.upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(reqs))
	}
	r := reqs[0]
	if r.Path != "/trading/api/retro/v1/retro_decisions" || r.Token != testToken {
		t.Errorf("request path/token = %s token-ok=%v", r.Path, r.Token == testToken)
	}
	want := map[string][]string{
		"from": {"2026-01-01"}, "to": {"2026-01-31"}, "market": {"us"},
		"symbol": {"SYN001"}, "limit": {"50"}, "cursor": {"synthetic_cursor"},
	}
	if !reflect.DeepEqual(r.Query, want) {
		t.Errorf("upstream query = %v, want %v", r.Query, want)
	}

	// Only from and to are required; the optional ones are then not sent.
	s2 := newRetroServer(t)
	if tr := s2.call(t, "retro_decisions", map[string]any{"from": "2026-01-01", "to": "2026-01-31"}); tr.IsError {
		t.Fatalf("tool error: %s", tr.text())
	}
	if q := s2.upstream.Requests()[0].Query; !reflect.DeepEqual(q, map[string][]string{"from": {"2026-01-01"}, "to": {"2026-01-31"}}) {
		t.Errorf("optional-free query = %v", q)
	}
}

// retro_decisions refuses an out-of-contract argument before the upstream is
// called, as the other tools do.
func TestRetroDecisionsRefusesInvalidArgumentsLocally(t *testing.T) {
	cases := map[string]map[string]any{
		"missing to":        {"from": "2026-01-01"},
		"bad market":        {"from": "2026-01-01", "to": "2026-01-02", "market": "jp"},
		"symbol with space": {"from": "2026-01-01", "to": "2026-01-02", "symbol": "SYN 001"},
		"symbol too long":   {"from": "2026-01-01", "to": "2026-01-02", "symbol": "ABCDEFGHIJKLMNOPQRSTUVWXYZABCDEFG"},
		"limit above 500":   {"from": "2026-01-01", "to": "2026-01-02", "limit": 501},
		"unknown argument":  {"from": "2026-01-01", "to": "2026-01-02", "account": "x"},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			s := newRetroServer(t)
			tr := s.call(t, "retro_decisions", a)
			if !tr.IsError {
				t.Errorf("accepted %v", a)
			}
			if n := len(s.upstream.Requests()); n != 0 {
				t.Errorf("upstream saw %d requests for an invalid call", n)
			}
		})
	}
}
