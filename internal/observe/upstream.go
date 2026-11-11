package observe

import (
	"context"
	"errors"
	"time"

	"github.com/mgh3326/factortrail/internal/providers/autotrader"
)

// Upstream is the provider surface the retro tools call. The provider
// satisfies it, and so does ObservedUpstream.
type Upstream interface {
	RetroDay(context.Context, *autotrader.RetroDayParams) (autotrader.Result, error)
	RetroSymbol(context.Context, *autotrader.RetroSymbolParams) (autotrader.Result, error)
	RetroFunnel(context.Context, *autotrader.RetroFunnelParams) (autotrader.Result, error)
	RetroScoreboard(context.Context, *autotrader.RetroScoreboardParams) (autotrader.Result, error)
	RetroPositions(context.Context, *autotrader.RetroPositionsParams) (autotrader.Result, error)
	MockExperiments(context.Context, *autotrader.MockExperimentsParams) (autotrader.Result, error)
	GetBars(context.Context, *autotrader.GetBarsParams) (autotrader.Result, error)
	DataGaps(context.Context, *autotrader.DataGapsParams) (autotrader.Result, error)
	RetroTrades(context.Context, *autotrader.RetroTradesParams) (autotrader.Result, error)
}

// ObservedUpstream counts and times every upstream call, and reports the
// provider failures worth an operator's attention to Sentry. It changes no
// result and no error.
type ObservedUpstream struct {
	next    Upstream
	metrics *Metrics
	errs    *Errors
}

// NewObservedUpstream wraps next.
func NewObservedUpstream(next Upstream, m *Metrics, e *Errors) *ObservedUpstream {
	return &ObservedUpstream{next: next, metrics: m, errs: e}
}

func (u *ObservedUpstream) RetroDay(ctx context.Context, q *autotrader.RetroDayParams) (autotrader.Result, error) {
	return u.call("retro_day", func() (autotrader.Result, error) { return u.next.RetroDay(ctx, q) })
}

func (u *ObservedUpstream) RetroSymbol(ctx context.Context, q *autotrader.RetroSymbolParams) (autotrader.Result, error) {
	return u.call("retro_symbol", func() (autotrader.Result, error) { return u.next.RetroSymbol(ctx, q) })
}

func (u *ObservedUpstream) RetroFunnel(ctx context.Context, q *autotrader.RetroFunnelParams) (autotrader.Result, error) {
	return u.call("retro_funnel", func() (autotrader.Result, error) { return u.next.RetroFunnel(ctx, q) })
}

func (u *ObservedUpstream) RetroScoreboard(ctx context.Context, q *autotrader.RetroScoreboardParams) (autotrader.Result, error) {
	return u.call("retro_scoreboard", func() (autotrader.Result, error) { return u.next.RetroScoreboard(ctx, q) })
}

func (u *ObservedUpstream) RetroPositions(ctx context.Context, q *autotrader.RetroPositionsParams) (autotrader.Result, error) {
	return u.call("retro_positions", func() (autotrader.Result, error) { return u.next.RetroPositions(ctx, q) })
}

func (u *ObservedUpstream) MockExperiments(ctx context.Context, q *autotrader.MockExperimentsParams) (autotrader.Result, error) {
	return u.call("mock_experiments", func() (autotrader.Result, error) { return u.next.MockExperiments(ctx, q) })
}

func (u *ObservedUpstream) GetBars(ctx context.Context, q *autotrader.GetBarsParams) (autotrader.Result, error) {
	return u.call("get_bars", func() (autotrader.Result, error) { return u.next.GetBars(ctx, q) })
}

func (u *ObservedUpstream) DataGaps(ctx context.Context, q *autotrader.DataGapsParams) (autotrader.Result, error) {
	return u.call("data_gaps", func() (autotrader.Result, error) { return u.next.DataGaps(ctx, q) })
}

func (u *ObservedUpstream) RetroTrades(ctx context.Context, q *autotrader.RetroTradesParams) (autotrader.Result, error) {
	return u.call("retro_trades", func() (autotrader.Result, error) { return u.next.RetroTrades(ctx, q) })
}

func (u *ObservedUpstream) call(op string, do func() (autotrader.Result, error)) (autotrader.Result, error) {
	start := time.Now()
	res, err := do()
	outcome := outcomeOf(err)
	u.metrics.observeUpstream(op, outcome, time.Since(start))
	if reportable(err) {
		u.errs.CaptureError(nil, err, map[string]string{"operation": op, "outcome": outcome})
	}
	return res, err
}

// outcomeOf names a call's result for the outcome label.
func outcomeOf(err error) string {
	if err == nil {
		return outcomeOK
	}
	var ce *autotrader.CallError
	if errors.As(err, &ce) {
		return string(ce.Code)
	}
	return labelOther
}

// reportable is true for the failures an operator must look at. Validation
// and feature-off answers are expected from callers and the upstream's own
// configuration, so they are counted but not reported.
func reportable(err error) bool {
	var ce *autotrader.CallError
	if !errors.As(err, &ce) {
		return false
	}
	switch ce.Code {
	case autotrader.ErrorCodeValidationError, autotrader.ErrorCodeFeatureOff:
		return false
	}
	return true
}
