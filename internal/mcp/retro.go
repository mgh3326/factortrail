package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	openapi_types "github.com/oapi-codegen/runtime/types"

	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mgh3326/factortrail/internal/providers/autotrader"
)

// Retro is the upstream the retro tools read from. *autotrader.Provider is
// the production implementation. Each tool calls exactly one of these
// methods, once; no tool argument selects the method, a URL or a path.
type Retro interface {
	RetroDay(context.Context, *autotrader.RetroDayParams) (autotrader.Result, error)
	RetroSymbol(context.Context, *autotrader.RetroSymbolParams) (autotrader.Result, error)
	RetroFunnel(context.Context, *autotrader.RetroFunnelParams) (autotrader.Result, error)
	RetroScoreboard(context.Context, *autotrader.RetroScoreboardParams) (autotrader.Result, error)
	RetroPositions(context.Context, *autotrader.RetroPositionsParams) (autotrader.Result, error)
	MockExperiments(context.Context, *autotrader.MockExperimentsParams) (autotrader.Result, error)
	GetBars(context.Context, *autotrader.GetBarsParams) (autotrader.Result, error)
	DataGaps(context.Context, *autotrader.DataGapsParams) (autotrader.Result, error)
}

// errNoUpstream is what a retro tool answers when the process started without
// the upstream settings.
var errNoUpstream = errors.New("retro upstream is not configured")

// Tool inputs. Each field is one query parameter of the pinned contract, with
// the same name and constraints (see the schema helpers below); the schemas
// are written out, not inferred, so they match the contract exactly.

type retroDayIn struct {
	Market string  `json:"market"`
	Date   string  `json:"date"`
	Limit  *int    `json:"limit,omitempty"`
	Cursor *string `json:"cursor,omitempty"`
}

type retroSymbolIn struct {
	Symbol string  `json:"symbol"`
	From   string  `json:"from"`
	To     string  `json:"to"`
	Limit  *int    `json:"limit,omitempty"`
	Cursor *string `json:"cursor,omitempty"`
}

type retroFunnelIn struct {
	Market string  `json:"market"`
	From   string  `json:"from"`
	To     string  `json:"to"`
	Limit  *int    `json:"limit,omitempty"`
	Cursor *string `json:"cursor,omitempty"`
}

type retroScoreboardIn struct {
	From   string  `json:"from"`
	To     string  `json:"to"`
	Limit  *int    `json:"limit,omitempty"`
	Cursor *string `json:"cursor,omitempty"`
}

type retroPositionsIn struct {
	CutoffDate string  `json:"cutoff_date"`
	Limit      *int    `json:"limit,omitempty"`
	Cursor     *string `json:"cursor,omitempty"`
}

type mockExperimentsIn struct {
	StrategyID *string `json:"strategy_id,omitempty"`
	Limit      *int    `json:"limit,omitempty"`
	Cursor     *string `json:"cursor,omitempty"`
}

type getBarsIn struct {
	Symbol string  `json:"symbol"`
	Tf     string  `json:"tf"`
	From   string  `json:"from"`
	To     string  `json:"to"`
	Limit  *int    `json:"limit,omitempty"`
	Cursor *string `json:"cursor,omitempty"`
}

type dataGapsIn struct {
	Limit  *int    `json:"limit,omitempty"`
	Cursor *string `json:"cursor,omitempty"`
}

// Schema helpers: the contract's parameter types.

func strProp(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: desc}
}

func dateProp(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Format: "date", Pattern: `^\d{4}-\d{2}-\d{2}$`, Description: desc}
}

func limitProp() *jsonschema.Schema {
	one, max := 1.0, 500.0
	return &jsonschema.Schema{Type: "integer", Minimum: &one, Maximum: &max,
		Description: "Rows per page, 1 to 500. The upstream default is 100."}
}

func cursorProp() *jsonschema.Schema {
	n := 128
	return &jsonschema.Schema{Type: "string", Pattern: `^[A-Za-z0-9_.:\-]+$`, MaxLength: &n,
		Description: "Opaque cursor from a previous response's next_cursor."}
}

// objectSchema builds a closed object schema: unknown arguments are refused.
func objectSchema(required []string, props map[string]*jsonschema.Schema) *jsonschema.Schema {
	props["limit"] = limitProp()
	props["cursor"] = cursorProp()
	return &jsonschema.Schema{
		Type:                 "object",
		Properties:           props,
		Required:             required,
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
	}
}

func parseDate(field, v string) (openapi_types.Date, error) {
	t, err := time.Parse(time.DateOnly, v)
	if err != nil {
		return openapi_types.Date{}, fmt.Errorf("%s must be a date as YYYY-MM-DD", field)
	}
	return openapi_types.Date{Time: t}, nil
}

// addRetroTool registers one read-only tool. call runs the tool's one fixed
// operation; its result is returned as the structured content unchanged.
func addRetroTool[In any](s *gmcp.Server, b Retro, name, desc string, schema *jsonschema.Schema,
	call func(context.Context, Retro, In) (autotrader.Result, error)) {
	openWorld := false
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        name,
		Description: desc,
		InputSchema: schema,
		Annotations: &gmcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &openWorld},
	}, func(ctx context.Context, _ *gmcp.CallToolRequest, in In) (*gmcp.CallToolResult, any, error) {
		if b == nil {
			return nil, nil, errNoUpstream
		}
		res, err := call(ctx, b, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, res, nil
	})
}

const retroNote = " Read-only. Unknown values stay null with a reason; they are never zero."

func registerRetroDay(s *gmcp.Server, b Retro) {
	addRetroTool(s, b, "retro_day",
		"One trading day's chain from candidate to order, fill and holding for a market."+retroNote,
		objectSchema([]string{"market", "date"}, map[string]*jsonschema.Schema{
			"market": strProp("Market code."),
			"date":   dateProp("Trading date, YYYY-MM-DD."),
		}),
		func(ctx context.Context, b Retro, in retroDayIn) (autotrader.Result, error) {
			d, err := parseDate("date", in.Date)
			if err != nil {
				return nil, err
			}
			return b.RetroDay(ctx, &autotrader.RetroDayParams{Market: in.Market, Date: d, Limit: in.Limit, Cursor: in.Cursor})
		})
}

func registerRetroSymbol(s *gmcp.Server, b Retro) {
	addRetroTool(s, b, "retro_symbol",
		"One symbol's entries, adds, partial sells and holdings over a date range of at most 366 days."+retroNote,
		objectSchema([]string{"symbol", "from", "to"}, map[string]*jsonschema.Schema{
			"symbol": strProp("Symbol."),
			"from":   dateProp("First date, YYYY-MM-DD."),
			"to":     dateProp("Last date, YYYY-MM-DD, inclusive."),
		}),
		func(ctx context.Context, b Retro, in retroSymbolIn) (autotrader.Result, error) {
			from, err := parseDate("from", in.From)
			if err != nil {
				return nil, err
			}
			to, err := parseDate("to", in.To)
			if err != nil {
				return nil, err
			}
			return b.RetroSymbol(ctx, &autotrader.RetroSymbolParams{Symbol: in.Symbol, From: from, To: to, Limit: in.Limit, Cursor: in.Cursor})
		})
}

func registerRetroFunnel(s *gmcp.Server, b Retro) {
	addRetroTool(s, b, "retro_funnel",
		"Stage counts and fill rates for one cohort of a market over a date range."+retroNote,
		objectSchema([]string{"market", "from", "to"}, map[string]*jsonschema.Schema{
			"market": strProp("Market code."),
			"from":   dateProp("First date, YYYY-MM-DD."),
			"to":     dateProp("Last date, YYYY-MM-DD, inclusive."),
		}),
		func(ctx context.Context, b Retro, in retroFunnelIn) (autotrader.Result, error) {
			from, err := parseDate("from", in.From)
			if err != nil {
				return nil, err
			}
			to, err := parseDate("to", in.To)
			if err != nil {
				return nil, err
			}
			return b.RetroFunnel(ctx, &autotrader.RetroFunnelParams{Market: in.Market, From: from, To: to, Limit: in.Limit, Cursor: in.Cursor})
		})
}

func registerRetroScoreboard(s *gmcp.Server, b Retro) {
	addRetroTool(s, b, "retro_scoreboard",
		"Account, market and sleeve performance over a date range, with the basis of each figure."+retroNote,
		objectSchema([]string{"from", "to"}, map[string]*jsonschema.Schema{
			"from": dateProp("First date, YYYY-MM-DD."),
			"to":   dateProp("Last date, YYYY-MM-DD, inclusive."),
		}),
		func(ctx context.Context, b Retro, in retroScoreboardIn) (autotrader.Result, error) {
			from, err := parseDate("from", in.From)
			if err != nil {
				return nil, err
			}
			to, err := parseDate("to", in.To)
			if err != nil {
				return nil, err
			}
			return b.RetroScoreboard(ctx, &autotrader.RetroScoreboardParams{From: from, To: to, Limit: in.Limit, Cursor: in.Cursor})
		})
}

func registerRetroPositions(s *gmcp.Server, b Retro) {
	addRetroTool(s, b, "retro_positions",
		"Positions held at a cutoff date, with cost basis and lot state."+retroNote,
		objectSchema([]string{"cutoff_date"}, map[string]*jsonschema.Schema{
			"cutoff_date": dateProp("Cutoff date, YYYY-MM-DD."),
		}),
		func(ctx context.Context, b Retro, in retroPositionsIn) (autotrader.Result, error) {
			d, err := parseDate("cutoff_date", in.CutoffDate)
			if err != nil {
				return nil, err
			}
			return b.RetroPositions(ctx, &autotrader.RetroPositionsParams{CutoffDate: d, Limit: in.Limit, Cursor: in.Cursor})
		})
}

func registerMockExperiments(s *gmcp.Server, b Retro) {
	addRetroTool(s, b, "mock_experiments",
		"Mock experiments and their planned, sent and filled counts."+retroNote,
		objectSchema(nil, map[string]*jsonschema.Schema{
			"strategy_id": strProp("Strategy identifier."),
		}),
		func(ctx context.Context, b Retro, in mockExperimentsIn) (autotrader.Result, error) {
			return b.MockExperiments(ctx, &autotrader.MockExperimentsParams{StrategyId: in.StrategyID, Limit: in.Limit, Cursor: in.Cursor})
		})
}

func registerGetBars(s *gmcp.Server, b Retro) {
	tf := strProp("Bar timeframe.")
	tf.Enum = []any{"1m", "5m", "60m", "1d"}
	addRetroTool(s, b, "get_bars",
		"Price bars for a symbol over a date range of at most 366 days, with source, venue and adjustment basis."+retroNote,
		objectSchema([]string{"symbol", "tf", "from", "to"}, map[string]*jsonschema.Schema{
			"symbol": strProp("Symbol."),
			"tf":     tf,
			"from":   dateProp("First date, YYYY-MM-DD."),
			"to":     dateProp("Last date, YYYY-MM-DD, inclusive."),
		}),
		func(ctx context.Context, b Retro, in getBarsIn) (autotrader.Result, error) {
			from, err := parseDate("from", in.From)
			if err != nil {
				return nil, err
			}
			to, err := parseDate("to", in.To)
			if err != nil {
				return nil, err
			}
			return b.GetBars(ctx, &autotrader.GetBarsParams{Symbol: in.Symbol, Tf: autotrader.GetBarsParamsTf(in.Tf), From: from, To: to, Limit: in.Limit, Cursor: in.Cursor})
		})
}

func registerDataGaps(s *gmcp.Server, b Retro) {
	addRetroTool(s, b, "data_gaps",
		"Datasets with missing, stale or unsupported data, so a conclusion can be judged as unsafe."+retroNote,
		objectSchema(nil, map[string]*jsonschema.Schema{}),
		func(ctx context.Context, b Retro, in dataGapsIn) (autotrader.Result, error) {
			return b.DataGaps(ctx, &autotrader.DataGapsParams{Limit: in.Limit, Cursor: in.Cursor})
		})
}
