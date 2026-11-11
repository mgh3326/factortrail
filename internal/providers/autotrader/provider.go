// Package autotrader is the Factortrail provider for the auto_trader
// retro/v1 read API. The HTTP client is generated from the pinned contract
// (see generate.go); this file is the thin hand-written layer around it.
//
// The package does not import the MCP server, so a CI job can run the pinned
// client against a candidate provider without it.
//
// What the layer guarantees:
//
//   - The base URL comes from Config only. A call chooses one of nine fixed
//     operations through a typed method; no argument is a URL, path or
//     operation name.
//   - The read token is read once from a file and sent only as the
//     X-Retro-Read-Token header. It is never part of an error, a log line or
//     a URL.
//   - Every call has a timeout and honours context cancellation, redirects are
//     refused, and a response larger than the cap is refused.
//   - A response body is returned as decoded JSON with numbers kept as
//     json.Number, so an integer above 2^53, a decimal string such as
//     "1.2300" and null reach the caller exactly as the provider sent them.
//   - An upstream failure becomes a *CallError carrying a contract error code and
//     the contract's fixed message. An upstream body is never echoed.
package autotrader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// TokenHeader is the header that carries the read token (security scheme
// RetroReadToken in the contract).
const TokenHeader = "X-Retro-Read-Token"

// Defaults for Config fields left at zero.
const (
	DefaultTimeout          = 15 * time.Second
	DefaultMaxResponseBytes = 4 << 20
)

// maxTokenFileBytes bounds the token file. A token is a short secret.
const maxTokenFileBytes = 4096

// Config configures a Provider.
type Config struct {
	// BaseURL is the upstream origin, for example "http://127.0.0.1:8000".
	// Scheme http or https and a host are required; user info, query and
	// fragment are refused.
	BaseURL string
	// TokenFile is the path of the file that holds the read token. It is read
	// once, by New.
	TokenFile string
	// Timeout bounds one call. Zero means DefaultTimeout.
	Timeout time.Duration
	// MaxResponseBytes bounds one response body. Zero means
	// DefaultMaxResponseBytes.
	MaxResponseBytes int64
	// Transport carries the requests. Nil means a fresh http.Transport that
	// ignores proxy environment variables. Tests inject a fake here.
	Transport http.RoundTripper
}

// messages are the contract's fixed messages, one per contract error code
// (ErrorCode and ErrorMessage are generated from components.schemas.Error).
var messages = map[ErrorCode]ErrorMessage{
	ErrorCodeAuthMissing:         ErrorMessageAuthenticationRequired,
	ErrorCodeAuthInvalid:         ErrorMessageAuthenticationInvalid,
	ErrorCodeScopeDenied:         ErrorMessageScopeDenied,
	ErrorCodeFeatureOff:          ErrorMessageFeatureUnavailable,
	ErrorCodeValidationError:     ErrorMessageRequestInvalid,
	ErrorCodeTimeout:             ErrorMessageRequestTimedOut,
	ErrorCodeUpstreamUnavailable: ErrorMessageUpstreamUnavailable,
	ErrorCodeSchemaMismatch:      ErrorMessageSchemaMismatch,
}

// CallError is the only error a call returns for an upstream or transport
// failure. Its text is fixed per code, so it can be shown to a tool caller
// and logged without leaking an upstream body or the token.
type CallError struct {
	Code ErrorCode
	// Status is the upstream HTTP status, or 0 when there was no response.
	Status int
}

func (e *CallError) Error() string {
	return "autotrader " + string(e.Code) + ": " + string(messages[e.Code])
}

// Provider calls the retro/v1 API. It is safe for concurrent use.
type Provider struct {
	client  *Client
	timeout time.Duration
	maxBody int64
}

// New validates cfg, reads the token file and returns a Provider. Errors name
// settings, never values.
func New(cfg Config) (*Provider, error) {
	base, err := parseBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	token, err := readToken(cfg.TokenFile)
	if err != nil {
		return nil, err
	}
	p := &Provider{timeout: cfg.Timeout, maxBody: cfg.MaxResponseBytes}
	if p.timeout <= 0 {
		p.timeout = DefaultTimeout
	}
	if p.maxBody <= 0 {
		p.maxBody = DefaultMaxResponseBytes
	}
	rt := cfg.Transport
	if rt == nil {
		rt = &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second}
	}
	httpClient := &http.Client{
		Transport: rt,
		// A redirect is returned to the caller as a 3xx response, which the
		// provider treats as a failure. The token is never sent elsewhere.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	p.client, err = NewClient(base,
		WithHTTPClient(httpClient),
		WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set(TokenHeader, token)
			req.Header.Set("Accept", "application/json")
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("upstream client: %w", err)
	}
	return p, nil
}

func parseBaseURL(raw string) (string, error) {
	const name = "retro upstream base URL"
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("%s must be an http or https URL with a host", name)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%s must use http or https", name)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("%s must not contain user info, a query or a fragment", name)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func readToken(path string) (string, error) {
	const name = "retro upstream token file"
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s is not set", name)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%s cannot be read", name)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxTokenFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("%s cannot be read", name)
	}
	if len(b) > maxTokenFileBytes {
		return "", fmt.Errorf("%s is too large", name)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("%s is empty", name)
	}
	for _, r := range token {
		if r < 0x21 || r > 0x7e {
			return "", fmt.Errorf("%s must hold one token of printable ASCII", name)
		}
	}
	return token, nil
}

// Result is a decoded 200 response: a JSON object whose numbers are
// json.Number.
type Result = map[string]any

// call runs one generated operation and turns its response into a Result or
// a *CallError. do is one of the generated Client methods, already bound to its
// parameters.
func (p *Provider) call(ctx context.Context, do func(context.Context) (*http.Response, error)) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	resp, err := do(ctx)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, p.maxBody+1))
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if int64(len(body)) > p.maxBody {
		return nil, &CallError{Code: ErrorCodeUpstreamUnavailable, Status: resp.StatusCode}
	}
	if resp.StatusCode == http.StatusOK {
		res, err := decodeObject(body)
		if err != nil {
			return nil, &CallError{Code: ErrorCodeSchemaMismatch, Status: resp.StatusCode}
		}
		return res, nil
	}
	return nil, statusError(resp.StatusCode, body)
}

// transportError maps a failure before a full response to a code. A
// cancelled caller gets context.Canceled back unchanged; the rest are
// timeout or upstream_unavailable.
func transportError(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		return &CallError{Code: ErrorCodeTimeout}
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return context.Canceled
	}
	return &CallError{Code: ErrorCodeUpstreamUnavailable}
}

// allowedCodes are the codes the contract lists per status.
var allowedCodes = map[int][]ErrorCode{
	http.StatusUnauthorized:        {ErrorCodeAuthMissing, ErrorCodeAuthInvalid},
	http.StatusForbidden:           {ErrorCodeScopeDenied},
	http.StatusNotFound:            {ErrorCodeFeatureOff},
	http.StatusUnprocessableEntity: {ErrorCodeValidationError},
	http.StatusServiceUnavailable:  {ErrorCodeTimeout, ErrorCodeUpstreamUnavailable, ErrorCodeSchemaMismatch},
}

// statusError maps a non-200 status to a contract code. The upstream body is
// read only to pick among the codes the contract allows for that status; it is
// never copied into the result.
func statusError(status int, body []byte) *CallError {
	allowed, known := allowedCodes[status]
	if !known {
		return &CallError{Code: ErrorCodeUpstreamUnavailable, Status: status}
	}
	var e struct {
		Code ErrorCode `json:"code"`
	}
	if json.Unmarshal(body, &e) == nil {
		for _, c := range allowed {
			if e.Code == c {
				return &CallError{Code: c, Status: status}
			}
		}
	}
	// Defaults when the body is missing or names a code the contract does not
	// allow for this status.
	switch status {
	case http.StatusUnauthorized:
		return &CallError{Code: ErrorCodeAuthInvalid, Status: status}
	case http.StatusForbidden:
		return &CallError{Code: ErrorCodeScopeDenied, Status: status}
	case http.StatusNotFound:
		return &CallError{Code: ErrorCodeFeatureOff, Status: status}
	case http.StatusUnprocessableEntity:
		return &CallError{Code: ErrorCodeValidationError, Status: status}
	}
	return &CallError{Code: ErrorCodeUpstreamUnavailable, Status: status}
}

// decodeObject decodes exactly one JSON object, keeping numbers as
// json.Number. Anything else, including trailing data, is an error.
func decodeObject(body []byte) (Result, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var res Result
	if err := dec.Decode(&res); err != nil {
		return nil, err
	}
	if res == nil {
		return nil, errors.New("not an object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return res, nil
}

// The nine operations. Each calls its one fixed generated method exactly
// once.

func (p *Provider) RetroDay(ctx context.Context, q *RetroDayParams) (Result, error) {
	return p.call(ctx, func(ctx context.Context) (*http.Response, error) { return p.client.RetroDay(ctx, q) })
}

func (p *Provider) RetroSymbol(ctx context.Context, q *RetroSymbolParams) (Result, error) {
	return p.call(ctx, func(ctx context.Context) (*http.Response, error) { return p.client.RetroSymbol(ctx, q) })
}

func (p *Provider) RetroFunnel(ctx context.Context, q *RetroFunnelParams) (Result, error) {
	return p.call(ctx, func(ctx context.Context) (*http.Response, error) { return p.client.RetroFunnel(ctx, q) })
}

func (p *Provider) RetroScoreboard(ctx context.Context, q *RetroScoreboardParams) (Result, error) {
	return p.call(ctx, func(ctx context.Context) (*http.Response, error) { return p.client.RetroScoreboard(ctx, q) })
}

func (p *Provider) RetroPositions(ctx context.Context, q *RetroPositionsParams) (Result, error) {
	return p.call(ctx, func(ctx context.Context) (*http.Response, error) { return p.client.RetroPositions(ctx, q) })
}

func (p *Provider) MockExperiments(ctx context.Context, q *MockExperimentsParams) (Result, error) {
	return p.call(ctx, func(ctx context.Context) (*http.Response, error) { return p.client.MockExperiments(ctx, q) })
}

func (p *Provider) GetBars(ctx context.Context, q *GetBarsParams) (Result, error) {
	return p.call(ctx, func(ctx context.Context) (*http.Response, error) { return p.client.GetBars(ctx, q) })
}

func (p *Provider) DataGaps(ctx context.Context, q *DataGapsParams) (Result, error) {
	return p.call(ctx, func(ctx context.Context) (*http.Response, error) { return p.client.DataGaps(ctx, q) })
}

func (p *Provider) RetroTrades(ctx context.Context, q *RetroTradesParams) (Result, error) {
	return p.call(ctx, func(ctx context.Context) (*http.Response, error) { return p.client.RetroTrades(ctx, q) })
}
