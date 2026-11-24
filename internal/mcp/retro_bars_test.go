package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/mgh3326/factortrail/internal/providers/autotrader/fake"
)

// A kr 1m bar's session_segment, first_received_at and value_reason reach the
// get_bars result unchanged, next to the fields the bar already had. value is
// absent because value_reason explains it.
func TestGetBars1mFieldsPassThroughUnchanged(t *testing.T) {
	const body = `{"as_of":"2026-01-02T09:00:00+09:00","data_as_of":"2026-01-02T09:00:00+09:00",` +
		`"timezone":"KST","total":1,"truncated":false,"limit":100,"cursor":null,"next_cursor":null,"unknowns":[],` +
		`"data":[{"symbol":"005930","time":"2026-01-02T09:01:00+09:00","open_price":"70000.0000",` +
		`"high_price":"70100.0000","low_price":"69950.0000","close_price":"70050.0000","quantity":"1200.0000",` +
		`"currency":"KRW","unit":"base_asset","market":"kr","source":"synthetic","venue":"XSYN","adjusted":false,` +
		`"missing_reason":null,"basis":"inference","session_segment":"KRX_REGULAR",` +
		`"first_received_at":"2026-01-02T09:01:03+09:00","value_reason":"missing_data"}]}`

	s := newRetroServer(t)
	s.upstream.Override = func(w http.ResponseWriter, _ *http.Request) { fake.WriteJSON(w, http.StatusOK, []byte(body)) }
	tr := s.call(t, "get_bars", map[string]any{
		"symbol": "005930", "market": "kr", "tf": "1m", "from": "2026-01-02", "to": "2026-01-02",
	})
	if tr.IsError {
		t.Fatalf("tool error: %s", tr.text())
	}
	if got, want := decodeAnyJSON(t, tr.StructuredContent), decodeAnyJSON(t, []byte(body)); !reflect.DeepEqual(got, want) {
		t.Fatalf("result differs from the upstream body\n got: %v\nwant: %v", got, want)
	}
	row := rowWithSymbol(t, rowsOf(t, tr.StructuredContent), "005930")
	for key, want := range map[string]string{
		"session_segment":   `"KRX_REGULAR"`,
		"first_received_at": `"2026-01-02T09:01:03+09:00"`,
		"value_reason":      `"missing_data"`,
	} {
		if got := string(row[key]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	if _, ok := row["value"]; ok {
		t.Error("value is present although value_reason explains it is absent")
	}
}

// decodeAnyJSON decodes JSON keeping numbers as json.Number, so two bodies
// compare by value and precision.
func decodeAnyJSON(t *testing.T, raw []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return v
}
