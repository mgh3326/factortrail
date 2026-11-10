package observe

import (
	"fmt"
	"net/http"
	"time"
)

// Instrument wraps the main listener's handler. It counts and times each
// request by fixed route, recovers a panic into a 500 response, and reports a
// panic or a 5xx to Sentry with the request shape, which Sentry scrubs.
//
// It does not change what the handler answers, except that a panic becomes a
// 500 with no detail.
func Instrument(next http.Handler, m *Metrics, e *Errors) http.Handler {
	return &instrumented{next: next, metrics: m, errs: e}
}

type instrumented struct {
	next    http.Handler
	metrics *Metrics
	errs    *Errors
}

func (h *instrumented) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := routeLabel(r.URL.Path)
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()
	defer func() {
		rec := recover()
		switch {
		case rec == http.ErrAbortHandler:
			h.metrics.observeHTTP(route, sw.status, time.Since(start))
			panic(rec)
		case rec != nil:
			if !sw.wrote {
				sw.status = http.StatusInternalServerError
				http.Error(sw, "internal error", http.StatusInternalServerError)
			}
			// The recovered value is not interpolated: it can carry anything the
			// handler was formatting, and no scrubber recognizes every credential
			// shape. The type is enough to triage.
			h.errs.CaptureError(r, fmt.Errorf("panic serving %s: %T", route, rec), map[string]string{"route": route})
		case sw.status >= 500:
			h.errs.CaptureError(r, fmt.Errorf("HTTP %d serving %s", sw.status, route), map[string]string{"route": route, "class": statusClass(sw.status)})
		}
		h.metrics.observeHTTP(route, sw.status, time.Since(start))
	}()
	h.next.ServeHTTP(sw, r)
}

// routeLabel maps a path to its fixed label.
func routeLabel(path string) string {
	switch path {
	case routeMCP, routeHealthz:
		return path
	default:
		return labelOther
	}
}

// statusWriter records the status the handler wrote. Unwrap and Flush keep
// http.ResponseController and streaming responses working.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush commits an implicit 200 in net/http, so the status is recorded as
// written before the flush goes through. A later panic can then no longer
// turn into a 500 for a response the client already received.
func (w *statusWriter) Flush() {
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
