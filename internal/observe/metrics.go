// Package observe is the service's telemetry: Prometheus metrics, Sentry
// error reporting, and the wrappers that feed them from the HTTP listener and
// the upstream provider.
//
// Both sinks are optional. The metrics are served only on the metrics
// listener, and Sentry is created only when a DSN is configured. Every label
// is drawn from a fixed set, so no request path, token, email or header value
// can become one.
package observe

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/mgh3326/factortrail/internal/providers/autotrader"
)

// Fixed label values. A route outside the two listed is labelled "other", and
// so is any value outside a fixed set.
const (
	routeMCP     = "/mcp"
	routeHealthz = "/healthz"
	labelOther   = "other"
)

// toolNames are the eight retro tools, the only values of the tool and
// operation labels. The mcp profile table is the source of truth; a name
// missing here is labelled "other", never added as a new value.
var toolNames = map[string]bool{
	"retro_day": true, "retro_symbol": true, "retro_funnel": true, "retro_scoreboard": true,
	"retro_positions": true, "mock_experiments": true, "get_bars": true, "data_gaps": true,
}

// outcomeOK labels a successful upstream call. Failures are labelled with the
// contract error code, and unknown failures with "other".
const outcomeOK = "ok"

var upstreamOutcomes = map[string]bool{
	outcomeOK:                                       true,
	string(autotrader.ErrorCodeAuthMissing):         true,
	string(autotrader.ErrorCodeAuthInvalid):         true,
	string(autotrader.ErrorCodeScopeDenied):         true,
	string(autotrader.ErrorCodeFeatureOff):          true,
	string(autotrader.ErrorCodeValidationError):     true,
	string(autotrader.ErrorCodeTimeout):             true,
	string(autotrader.ErrorCodeUpstreamUnavailable): true,
	string(autotrader.ErrorCodeSchemaMismatch):      true,
}

// Metrics holds the service's collectors on a private registry. The
// /metrics output carries exactly the series defined here.
type Metrics struct {
	reg          *prometheus.Registry
	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	toolCalls    *prometheus.CounterVec
	toolDuration *prometheus.HistogramVec
	upstreamReqs *prometheus.CounterVec
	upstreamDur  *prometheus.HistogramVec
}

// NewMetrics registers the collectors on a new registry.
func NewMetrics() *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "factortrail_http_requests_total",
			Help: "Requests served on the main listener, by route and status class.",
		}, []string{"route", "class"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "factortrail_http_request_duration_seconds",
			Help: "Time to serve a request on the main listener, by route.",
		}, []string{"route"}),
		toolCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "factortrail_mcp_tool_calls_total",
			Help: "MCP tool calls of the active profile, by tool and result.",
		}, []string{"tool", "result"}),
		toolDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "factortrail_mcp_tool_duration_seconds",
			Help: "Time of an MCP tool call, by tool.",
		}, []string{"tool"}),
		upstreamReqs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "factortrail_upstream_requests_total",
			Help: "Calls to the auto_trader retro/v1 upstream, by operation and outcome.",
		}, []string{"operation", "outcome"}),
		upstreamDur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "factortrail_upstream_request_duration_seconds",
			Help: "Time of a call to the auto_trader retro/v1 upstream, by operation.",
		}, []string{"operation"}),
	}
	m.reg.MustRegister(
		m.httpRequests, m.httpDuration,
		m.toolCalls, m.toolDuration,
		m.upstreamReqs, m.upstreamDur,
	)
	return m
}

// Handler serves the exposition format of the registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{Registry: m.reg})
}

func (m *Metrics) observeHTTP(route string, status int, d time.Duration) {
	m.httpRequests.WithLabelValues(route, statusClass(status)).Inc()
	m.httpDuration.WithLabelValues(route).Observe(d.Seconds())
}

// ObserveTool records one MCP tool call. It is the mcp.Options.OnToolCall hook.
func (m *Metrics) ObserveTool(tool string, d time.Duration, failed bool) {
	tool = fixedOr(tool, toolNames)
	result := outcomeOK
	if failed {
		result = "error"
	}
	m.toolCalls.WithLabelValues(tool, result).Inc()
	m.toolDuration.WithLabelValues(tool).Observe(d.Seconds())
}

func (m *Metrics) observeUpstream(op, outcome string, d time.Duration) {
	op = fixedOr(op, toolNames)
	outcome = fixedOr(outcome, upstreamOutcomes)
	m.upstreamReqs.WithLabelValues(op, outcome).Inc()
	m.upstreamDur.WithLabelValues(op).Observe(d.Seconds())
}

// fixedOr returns v when it is in the fixed set and "other" otherwise.
func fixedOr[V any](v string, set map[string]V) string {
	if _, ok := set[v]; ok {
		return v
	}
	return labelOther
}

func statusClass(status int) string {
	switch {
	case status >= 100 && status < 600:
		return string(rune('0'+status/100)) + "xx"
	default:
		return labelOther
	}
}
