// SPDX-License-Identifier: Apache-2.0

// Package httpapi exposes the service over HTTP as a Traefik ForwardAuth endpoint:
// POST/GET /check for the decision, GET /healthz, GET /metrics.
//
// A 2xx from /check lets the request through; any other status is returned to the
// client verbatim. The original request comes from X-Forwarded-*, not from the URL
// we are called on.
//
// # Why /healthz must not check Redis
//
// As a sidecar in the Traefik pods, a pod is Ready only when every container is.
// A Redis-aware probe would pull every Traefik pod from the load balancer during a
// Redis outage, turning a degraded abuse control into a total edge outage. Redis
// health goes to metrics instead, never to readiness.
package httpapi

import (
	"io"
	"log/slog"
	"math"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/dgamo/forwardlimit/internal/limiter"
	"github.com/dgamo/forwardlimit/internal/observability"
)

// Traefik ForwardAuth headers describing the original request.
const (
	headerForwardedURI    = "X-Forwarded-Uri"
	headerForwardedMethod = "X-Forwarded-Method"
)

// Response is what a client receives when a limiter rejects.
//
// Raw bytes rather than a structure: the payload is a client-facing contract the
// operator controls, and this way the hot path writes it without marshalling.
type Response struct {
	Status      int
	ContentType string
	Body        []byte
}

// Options configures the handler.
type Options struct {
	Engine  *limiter.Engine
	Metrics *observability.Metrics
	Logger  *slog.Logger

	// Response is sent when a limiter rejects and has no override.
	Response Response
	// Responses holds per-limiter overrides, keyed by limiter name.
	Responses map[string]Response

	// MaxBodyBytes bounds how much request body is read. It must stay ABOVE the
	// application's own body limit: if it were lower, a caller could pad the body
	// past it, break JSON parsing here, and evade body-keyed limits while the
	// application still accepted the request.
	MaxBodyBytes int64
}

// Handler serves the service's HTTP endpoints.
type Handler struct {
	engine    *limiter.Engine
	metrics   *observability.Metrics
	log       *slog.Logger
	maxBody   int64
	response  Response
	responses map[string]Response
	mux       *http.ServeMux
}

// New builds the handler.
func New(opts Options) *Handler {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = 262144
	}

	response := opts.Response
	if response.Status == 0 {
		response.Status = http.StatusTooManyRequests
	}
	if len(response.Body) == 0 {
		response.Body = []byte(`{"error":"rate_limited"}`)
		if response.ContentType == "" {
			response.ContentType = "application/json"
		}
	}

	h := &Handler{
		engine:    opts.Engine,
		metrics:   opts.Metrics,
		log:       log,
		maxBody:   maxBody,
		response:  response,
		responses: opts.Responses,
		mux:       http.NewServeMux(),
	}

	// Any method, since preserving the original one is a ForwardAuth setting. The
	// recovery wrapper is not optional: ForwardAuth returns any non-2xx to the
	// client, so an unhandled panic here would refuse the request.
	h.mux.HandleFunc("/check", recoverAndAllow(h.log, h.metrics, h.handleCheck))
	h.mux.HandleFunc("GET /healthz", h.handleHealth)
	if opts.Metrics != nil {
		h.mux.Handle("GET /metrics", opts.Metrics.Handler())
	}

	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// allowWriter tracks whether a status line has been sent, so recovery can tell
// whether it is still able to write one.
type allowWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *allowWriter) WriteHeader(code int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *allowWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// recoverAndAllow turns a panic into an allow.
//
// Every deliberate failure path here allows the request, and a bug must behave the
// same way: an unhandled panic writes no response, the proxy reads that as failure
// and returns 5xx, and a defect in an abuse control has refused a good request.
// Logged at error level and counted as failed_open, so it is not silent.
func recoverAndAllow(log *slog.Logger, metrics *observability.Metrics, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		aw := &allowWriter{ResponseWriter: w}

		defer func() {
			rec := recover()
			if rec == nil {
				return
			}

			log.Error("panic while checking request, allowing it through",
				slog.Any("panic", rec),
				slog.String("stack", string(debug.Stack())))
			if metrics != nil {
				metrics.RecordDecision("panic", string(limiter.VerdictFailedOpen))
				metrics.RecordStoreError()
			}

			// If a status has already gone out there is nothing left to correct;
			// the response is whatever was partially written.
			if !aw.wrote {
				aw.ResponseWriter.WriteHeader(http.StatusOK)
			}
		}()

		next(aw, r)
	}
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) handleCheck(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		if h.metrics != nil {
			h.metrics.ObserveCheck(time.Since(start))
		}
	}()

	body, truncated := h.readBody(r)
	if truncated {
		// Visible rather than silent: a truncated body cannot be inspected, so
		// body-keyed limiters will not apply to this request.
		h.log.Warn("request body exceeded MAX_BODY_BYTES; body-keyed limits cannot be applied",
			slog.Int64("max_body_bytes", h.maxBody),
			slog.String("path", originalPath(r)))
		if h.metrics != nil {
			h.metrics.RecordDecision("body", string(limiter.VerdictFailedOpen))
		}
	}

	req := &limiter.Request{
		Method:   originalMethod(r),
		Path:     originalPath(r),
		Header:   r.Header,
		Body:     body,
		RawQuery: originalQuery(r),
	}

	d := h.engine.Evaluate(r.Context(), req)
	h.record(d)

	if d.Blocked {
		msg := "request blocked"
		if d.DryRun {
			msg = "request would have been blocked (dry run, allowed through)"
		}
		h.log.Warn(msg,
			slog.String("limiter", d.Limiter),
			slog.String("path", req.Path),
			slog.Int64("count", d.Count),
			slog.Duration("retry_after", d.RetryAfter),
			slog.Bool("dry_run", d.DryRun))
	}

	// Only an enforced verdict rejects. A dry-run limiter has already been logged
	// and counted above, and the request proceeds.
	if d.Rejected() {
		h.writeBlocked(w, d)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// writeBlocked emits the rejection.
//
// ForwardAuth returns this to the client verbatim, so everything in it is public:
// no X-RateLimit-*, nothing naming the limiter or bucket, and no timing hint unless
// the limiter deliberately advised one.
func (h *Handler) writeBlocked(w http.ResponseWriter, d limiter.Decision) {
	resp := h.response
	if override, ok := h.responses[d.Limiter]; ok {
		resp = override
	}

	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	}
	if d.AdviseRetryAfter {
		w.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds(d.RetryAfter), 10))
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

func (h *Handler) record(d limiter.Decision) {
	if h.metrics == nil {
		return
	}

	if len(d.Evaluations) == 0 {
		h.metrics.RecordDecision("", string(limiter.VerdictAllowed))
		return
	}
	for _, e := range d.Evaluations {
		h.metrics.RecordDecision(e.Limiter, string(e.Verdict))
		if e.Verdict == limiter.VerdictFailedOpen {
			h.metrics.RecordStoreError()
		}
	}
}

// readBody reads at most maxBody bytes, reporting whether more was available.
func (h *Handler) readBody(r *http.Request) (body []byte, truncated bool) {
	if r.Body == nil {
		return nil, false
	}
	// Read one extra byte so truncation can be detected rather than inferred.
	b, err := io.ReadAll(io.LimitReader(r.Body, h.maxBody+1))
	if err != nil {
		h.log.Warn("failed to read request body", slog.String("error", err.Error()))
		return nil, false
	}
	if int64(len(b)) > h.maxBody {
		return b[:h.maxBody], true
	}
	return b, false
}

// originalPath returns the path Traefik is authorising, without the query string.
// Falls back to the received URL, which is what makes curl against /check work.
func originalPath(r *http.Request) string {
	uri := r.Header.Get(headerForwardedURI)
	if uri == "" {
		return r.URL.Path
	}
	if i := strings.IndexByte(uri, '?'); i >= 0 {
		uri = uri[:i]
	}
	return uri
}

// retryAfterSeconds renders Retry-After, floored at one second so a sub-second
// wait does not become "0" and invite an immediate retry.
func retryAfterSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 1
	}
	return int64(math.Ceil(d.Seconds()))
}

// originalQuery returns the query string being authorised. It must come from the
// forwarded URI, since a credential may arrive as a query parameter.
func originalQuery(r *http.Request) string {
	uri := r.Header.Get(headerForwardedURI)
	if uri == "" {
		return r.URL.RawQuery
	}
	if i := strings.IndexByte(uri, '?'); i >= 0 {
		return uri[i+1:]
	}
	return ""
}

func originalMethod(r *http.Request) string {
	if m := r.Header.Get(headerForwardedMethod); m != "" {
		return m
	}
	return r.Method
}
