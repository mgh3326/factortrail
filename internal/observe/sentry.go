package observe

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
)

// ErrInvalidDSN is returned when the DSN setting does not parse. Its text
// names the setting and never the value.
var ErrInvalidDSN = errors.New("sentry: the DSN setting is not a valid Sentry DSN")

// SentryOptions configures error reporting.
type SentryOptions struct {
	// DSN turns reporting on. Empty creates no client and sends nothing, even
	// when SENTRY_DSN is set in the environment.
	DSN         string
	Environment string
	Release     string
	// Redact lists literal values that must not appear in any event text, such
	// as the token file path, the upstream base URL and the Access settings.
	Redact []string
	// Transport carries the events. Nil means the SDK's HTTP transport. Tests
	// inject a fake here.
	Transport sentry.Transport
}

// Errors reports errors to Sentry. A nil *Errors is valid and reports nothing,
// which is the state when no DSN is configured.
type Errors struct {
	client *sentry.Client
	redact []string
}

// NewErrors returns nil and no error when opts.DSN is empty. Otherwise it
// returns a reporter whose events are scrubbed before they leave the process:
// no request headers that carry credentials, no cookies, no query strings, no
// user identity, no breadcrumbs, no tracing, and no token, JWT, email or
// configured literal in any text.
func NewErrors(opts SentryOptions) (*Errors, error) {
	if opts.DSN == "" {
		return nil, nil
	}
	redact := nonEmpty(opts.Redact)
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn:              opts.DSN,
		Environment:      opts.Environment,
		Release:          opts.Release,
		AttachStacktrace: true,
		SendDefaultPII:   false,
		EnableTracing:    false,
		TracesSampleRate: 0,
		Transport:        opts.Transport,
		BeforeSend: func(ev *sentry.Event, _ *sentry.EventHint) *sentry.Event {
			return scrubEvent(ev, redact)
		},
		BeforeBreadcrumb: func(*sentry.Breadcrumb, *sentry.BreadcrumbHint) *sentry.Breadcrumb {
			return nil
		},
	})
	if err != nil {
		return nil, ErrInvalidDSN
	}
	return &Errors{client: client, redact: redact}, nil
}

// CaptureError reports err with fixed tags. r, when non-nil, adds the request
// shape, which scrubEvent then cleans. Nothing is sent when e is nil.
func (e *Errors) CaptureError(r *http.Request, err error, tags map[string]string) {
	if e == nil || err == nil {
		return
	}
	scope := sentry.NewScope()
	for k, v := range tags {
		scope.SetTag(k, v)
	}
	if r != nil {
		scope.SetRequest(r)
	}
	e.client.CaptureException(err, &sentry.EventHint{}, scope)
}

// Flush waits up to timeout for queued events to be sent.
func (e *Errors) Flush(timeout time.Duration) {
	if e == nil {
		return
	}
	e.client.Flush(timeout)
}

var (
	jwtRE   = regexp.MustCompile(`eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]*\.[A-Za-z0-9_-]*`)
	emailRE = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
)

const redacted = "[redacted]"

// sensitiveHeaders are lower-cased request header names that carry a
// credential or a session.
var sensitiveHeaders = map[string]bool{
	"authorization":           true,
	"cookie":                  true,
	"set-cookie":              true,
	"cf-access-jwt-assertion": true,
	"x-retro-read-token":      true,
}

// scrubEvent removes what must not leave the process and returns the event.
func scrubEvent(ev *sentry.Event, literals []string) *sentry.Event {
	ev.User = sentry.User{}
	ev.ServerName = ""
	ev.Breadcrumbs = nil
	ev.Extra = nil
	ev.Message = scrubText(ev.Message, literals)
	for i := range ev.Exception {
		ev.Exception[i].Value = scrubText(ev.Exception[i].Value, literals)
	}
	for k, v := range ev.Tags {
		ev.Tags[k] = scrubText(v, literals)
	}
	if req := ev.Request; req != nil {
		// Sentry copies ordinary headers verbatim when SendDefaultPII is off,
		// so every retained key and value is scrubbed as text, not only the
		// credential names removed here.
		headers := make(map[string]string, len(req.Headers))
		for name, value := range req.Headers {
			if sensitiveHeaders[strings.ToLower(name)] {
				continue
			}
			headers[scrubText(name, literals)] = scrubText(value, literals)
		}
		req.Headers = headers
		// Env (REMOTE_ADDR and similar) is client identity, not needed here.
		req.Env = nil
		// The URL is dropped, not trimmed: a path can carry anything a caller
		// sends, and the route is already a fixed tag on the event.
		req.URL = ""
		req.Cookies = ""
		req.QueryString = ""
		req.Data = ""
	}
	return ev
}

// scrubText replaces each configured literal, every JWT and every email
// address with a fixed marker.
func scrubText(s string, literals []string) string {
	for _, lit := range literals {
		s = strings.ReplaceAll(s, lit, redacted)
	}
	s = jwtRE.ReplaceAllString(s, redacted)
	return emailRE.ReplaceAllString(s, redacted)
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
