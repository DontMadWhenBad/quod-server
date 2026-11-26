// Package server exposes the job manager over HTTP:
//
//	POST /v1/jobs                {input, options}       → 202 job view
//	GET  /v1/jobs                                       → job summaries, newest first
//	GET  /v1/jobs/{id}[?detail=full]                    → job view
//	GET  /v1/jobs/{id}/events[?after=N][&detail=full]   → SSE stream (resumable)
//	POST /v1/jobs/{id}/confirm   {stmt, accept, correction}
//	POST /v1/jobs/{id}/cancel
//	GET  /healthz, /readyz
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"mathprover/internal/auth"
	"mathprover/internal/jobs"
	"mathprover/internal/pipeline"
)

// Server serves the API. Set Manager once Lean is ready; until then job
// endpoints return 503.
type Server struct {
	// Auth identifies callers of /v1 endpoints; nil disables authentication
	// (every caller is auth.Anonymous; development only). Jobs are visible
	// only to the principal that created them.
	Auth      auth.Authenticator
	Log       *slog.Logger  // default slog.Default()
	Heartbeat time.Duration // SSE keep-alive interval; default 15s

	manager atomic.Pointer[jobs.Manager]
}

// SetManager makes the server ready.
func (s *Server) SetManager(m *jobs.Manager) { s.manager.Store(m) }

// Handler returns the routed handler with middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("POST /v1/jobs", s.withManager(s.createJob))
	mux.HandleFunc("GET /v1/jobs", s.withManager(s.listJobs))
	mux.HandleFunc("GET /v1/jobs/{id}", s.withManager(s.getJob))
	mux.HandleFunc("GET /v1/jobs/{id}/events", s.withManager(s.events))
	mux.HandleFunc("POST /v1/jobs/{id}/confirm", s.withManager(s.confirm))
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", s.withManager(s.cancel))
	return s.recoverer(s.logger(s.auth(mux)))
}

func (s *Server) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// ---------------------------------------------------------------- handlers

type managerHandler func(http.ResponseWriter, *http.Request, *jobs.Manager)

func (s *Server) withManager(h managerHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := s.manager.Load()
		if m == nil {
			writeError(w, http.StatusServiceUnavailable, "not_ready", "Lean workers are still starting")
			return
		}
		h(w, r, m)
	}
}

// principal is the authenticated caller (set by the auth middleware).
func principal(r *http.Request) *auth.Principal {
	if p := auth.FromContext(r.Context()); p != nil {
		return p
	}
	return auth.Anonymous
}

// ownedJob loads the job in the path if the caller owns it. Other callers'
// jobs are reported as not found, so job ids leak nothing.
func ownedJob(w http.ResponseWriter, r *http.Request, m *jobs.Manager) (*jobs.Job, bool) {
	j, err := m.Get(r.PathValue("id"))
	if err == nil && j.Owner.ID != principal(r).ID {
		err = jobs.ErrNotFound
	}
	if err != nil {
		writeManagerError(w, err)
		return nil, false
	}
	return j, true
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	m := s.manager.Load()
	if m == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true, "stats": m.Stats()})
}

type createRequest struct {
	Input   string       `json:"input"`
	Options jobs.Options `json:"options"`
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request, m *jobs.Manager) {
	var req createRequest
	if !readJSON(w, r, int64(m.Config().MaxInputBytes)+4096, &req) {
		return
	}
	j, err := m.Submit(req.Input, req.Options, principal(r))
	if err != nil {
		writeManagerError(w, err)
		return
	}
	w.Header().Set("Location", "/v1/jobs/"+j.ID)
	writeJSON(w, http.StatusAccepted, j.View(false))
}

type jobSummary struct {
	ID       string     `json:"id"`
	State    jobs.State `json:"state"`
	Input    string     `json:"input"`
	Created  time.Time  `json:"created"`
	Proved   int        `json:"proved"`
	Total    int        `json:"total"`
	CostUSD  float64    `json:"cost_usd"`
	Awaiting bool       `json:"awaiting_confirmation,omitempty"`
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request, m *jobs.Manager) {
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, 500)
	}
	out := []jobSummary{}
	for _, j := range m.List(principal(r).ID) {
		if len(out) == limit {
			break
		}
		v := j.View(false)
		sum := jobSummary{ID: v.ID, State: v.State, Input: truncate(v.Input, 200), Created: v.Created,
			Total: len(v.Statements), CostUSD: v.CostUSD, Awaiting: v.Pending != nil}
		for _, st := range v.Statements {
			if st.Status == pipeline.StatusProved {
				sum.Proved++
			}
		}
		out = append(out, sum)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request, m *jobs.Manager) {
	j, ok := ownedJob(w, r, m)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, j.View(r.URL.Query().Get("detail") == "full"))
}

type confirmRequest struct {
	Stmt       int    `json:"stmt"`
	Accept     bool   `json:"accept"`
	Correction string `json:"correction"`
}

func (s *Server) confirm(w http.ResponseWriter, r *http.Request, m *jobs.Manager) {
	var req confirmRequest
	if !readJSON(w, r, 16<<10, &req) {
		return
	}
	if req.Stmt <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "stmt (1-based statement index) is required")
		return
	}
	j, ok := ownedJob(w, r, m)
	if !ok {
		return
	}
	if err := m.Confirm(j.ID, req.Stmt, pipeline.Decision{Accept: req.Accept, Correction: req.Correction}); err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, j.View(false))
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request, m *jobs.Manager) {
	j, ok := ownedJob(w, r, m)
	if !ok {
		return
	}
	if err := m.Cancel(j.ID); err != nil {
		writeManagerError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, j.View(false))
}

// events streams job events as SSE. Clients resume with the standard
// Last-Event-ID header (sent automatically by EventSource) or ?after=N.
// The stream ends after the job's final event.
func (s *Server) events(w http.ResponseWriter, r *http.Request, m *jobs.Manager) {
	j, ok := ownedJob(w, r, m)
	if !ok {
		return
	}
	after := 0
	for _, v := range []string{r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after")} {
		if n, err := strconv.Atoi(v); err == nil && n > after {
			after = n
		}
	}
	full := r.URL.Query().Get("detail") == "full"

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n\n")
	if err := rc.Flush(); err != nil {
		return
	}

	hb := s.Heartbeat
	if hb <= 0 {
		hb = 15 * time.Second
	}
	ticker := time.NewTicker(hb)
	defer ticker.Stop()
	for {
		evs, changed, terminal := j.Since(after)
		for _, e := range evs {
			if !full {
				e = e.WithoutFeedback()
			}
			data, _ := json.Marshal(e)
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, data); err != nil {
				return
			}
			after = e.Seq
		}
		if len(evs) > 0 {
			if err := rc.Flush(); err != nil {
				return
			}
		}
		if terminal && len(evs) == 0 {
			fmt.Fprint(w, "event: end\ndata: {}\n\n")
			_ = rc.Flush()
			return
		}
		if terminal {
			continue // one more pass to confirm nothing is left, then end
		}
		select {
		case <-changed:
		case <-ticker.C:
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// ---------------------------------------------------------------- middleware

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		p := auth.Anonymous
		if s.Auth != nil {
			var err error
			p, err = s.Auth.Authenticate(r)
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="quod"`)
				msg := "missing access key (Authorization: Bearer qk_...)"
				if errors.Is(err, auth.ErrInvalid) {
					msg = "invalid or revoked access key"
				}
				writeError(w, http.StatusUnauthorized, "unauthorized", msg)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach Flush/SetWriteDeadline.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log().Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"dur", time.Since(t0).Round(time.Millisecond))
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log().Error("panic", "err", v, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- JSON helpers

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var b errorBody
	b.Error.Code, b.Error.Message = code, msg
	writeJSON(w, status, b)
}

func writeManagerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, jobs.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, jobs.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, jobs.ErrNoPending):
		writeError(w, http.StatusConflict, "no_pending_confirmation", err.Error())
	case errors.Is(err, jobs.ErrQueueFull):
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusTooManyRequests, "queue_full", err.Error())
	case errors.Is(err, jobs.ErrDailyBudget):
		writeError(w, http.StatusTooManyRequests, "daily_budget_exhausted", err.Error())
	case errors.Is(err, jobs.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, "shutting_down", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// readJSON decodes a size-limited JSON body; on failure it writes a 4xx.
func readJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "use Content-Type: application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("body exceeds %d bytes", limit))
		} else {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		}
		return false
	}
	return true
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// ShutdownContext is a helper for graceful stop: it returns a context that
// is canceled when the http.Server shuts down, for use as BaseContext so
// long-lived SSE requests end promptly.
func ShutdownContext(srv *http.Server) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	srv.RegisterOnShutdown(cancel)
	return ctx
}
