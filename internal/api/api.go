// Package api is the HTTP surface: bearer-key auth with per-key template
// allowlists, the async job API, the audited synchronous /render path,
// and the unauthenticated health/metrics endpoints.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/pavankomma/TypstPdf/internal/render"
	"github.com/pavankomma/TypstPdf/internal/sign"
	"github.com/pavankomma/TypstPdf/internal/store"
	"github.com/pavankomma/TypstPdf/internal/worker"
)

const maxBodyBytes = 20 << 20 // 20 MiB of JSON input per request

type Server struct {
	Store        *store.Store
	Renderer     worker.Renderer // sync /render path
	Pool         *worker.Pool    // Nudge on enqueue; StoreArtifact for sync renders
	Signer       *sign.Signer
	TemplatesDir string
	// ListTemplates enumerates available template names (the renderer's
	// Templates()); also the existence check for job submission.
	ListTemplates func() ([]string, error)
	TypstVersion  string
	MaxAttempts   int
	// NoAuth skips bearer auth; requests act as AnonKey (dev mode only).
	NoAuth  bool
	AnonKey *store.APIKey

	// Designer enables the template-manager API + UI (trusted envs only:
	// its preview endpoint compiles arbitrary Typst source).
	Designer       bool
	SourceRenderer SourceRenderer
	ExamplesDir    string
	DesignerUI     fs.FS // built SPA assets; nil = API only
}

type ctxKey int

const keyCtx ctxKey = 0

func callerKey(r *http.Request) *store.APIKey {
	k, _ := r.Context().Value(keyCtx).(*store.APIKey)
	return k
}

// Handler assembles the full route table with middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "typst": s.TypstVersion})
	})
	mux.Handle("GET /metrics", promhttp.Handler())

	mux.Handle("POST /v1/jobs", s.authed(s.postJob))
	mux.Handle("GET /v1/jobs", s.authed(s.listJobs))
	mux.Handle("GET /v1/jobs/{id}", s.authed(s.getJob))
	mux.Handle("GET /v1/jobs/{id}/pdf", s.authed(s.getJobPDF))
	mux.Handle("POST /render/{template}", s.authed(s.syncRender))
	mux.Handle("GET /templates", s.authed(s.getTemplates))
	mux.Handle("GET /v1/signing-key", s.authed(s.getSigningKey))

	if s.Designer {
		s.registerDesigner(mux)
	}

	return s.observe(mux)
}

// ---- middleware -------------------------------------------------------

// authed resolves the bearer key (or the anonymous dev identity) and puts
// it on the request context.
func (s *Server) authed(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var key *store.APIKey
		if s.NoAuth {
			key = s.AnonKey
		} else {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || token == "" {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing bearer token"})
				return
			}
			k, err := s.Store.LookupKey(strings.TrimSpace(token))
			if err != nil {
				if errors.Is(err, store.ErrUnauthorized) {
					writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or disabled API key"})
					return
				}
				slog.Error("key lookup", "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			key = k
		}
		next(w, r.WithContext(context.WithValue(r.Context(), keyCtx, key)))
	})
}

// statusWriter captures the response code for logs and metrics.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// observe wraps the mux with request logging and HTTP metrics.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		pattern := r.Pattern
		if pattern == "" {
			pattern = "(unmatched)"
		}
		httpDuration.WithLabelValues(r.Method, pattern, strconv.Itoa(sw.status)).
			Observe(time.Since(start).Seconds())
		if pattern == "GET /healthz" || pattern == "GET /metrics" {
			return // don't spam logs with probe traffic
		}
		attrs := []any{
			"method", r.Method, "path", r.URL.Path,
			"status", sw.status, "duration", time.Since(start).Round(time.Millisecond).String(),
		}
		if k := callerKey(r); k != nil {
			attrs = append(attrs, "key", k.Name)
		}
		slog.Info("http", attrs...)
	})
}

// ---- job API ----------------------------------------------------------

type jobRequest struct {
	Template string          `json:"template"`
	Data     json.RawMessage `json:"data"`
	Filename string          `json:"filename"`
}

// jobJSON is the wire shape of a job row (request data omitted unless
// ?include=data).
type jobJSON struct {
	ID               string     `json:"id"`
	Template         string     `json:"template"`
	TemplateVersion  string     `json:"template_version,omitempty"`
	Status           string     `json:"status"`
	Sync             bool       `json:"sync,omitempty"`
	Attempts         int        `json:"attempts"`
	MaxAttempts      int        `json:"max_attempts"`
	Error            string     `json:"error,omitempty"`
	DataSha256       string     `json:"data_sha256"`
	Filename         string     `json:"filename,omitempty"`
	PDFSha256        string     `json:"pdf_sha256,omitempty"`
	PDFBytes         int64      `json:"pdf_bytes,omitempty"`
	PDFStandard      string     `json:"pdf_standard,omitempty"`
	ArchivalFallback bool       `json:"archival_fallback,omitempty"`
	Signature        string     `json:"signature,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	Data             json.RawMessage `json:"data,omitempty"`
}

func toJobJSON(j *store.Job, includeData bool) jobJSON {
	out := jobJSON{
		ID: j.ID, Template: j.Template, TemplateVersion: j.TemplateVersion,
		Status: j.Status, Sync: j.Sync, Attempts: j.Attempts, MaxAttempts: j.MaxAttempts,
		Error: j.Error, DataSha256: j.DataSha256, Filename: j.Filename,
		PDFSha256: j.PDFSha256, PDFBytes: j.PDFBytes, PDFStandard: j.PDFStandard,
		ArchivalFallback: j.ArchivalFallback, Signature: j.Signature,
		CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt,
	}
	if includeData {
		out.Data = json.RawMessage(j.Data)
	}
	return out
}

// checkTemplate validates the requested template against existence and
// the caller's allowlist; writes the error response itself on failure.
func (s *Server) checkTemplate(w http.ResponseWriter, key *store.APIKey, name string) bool {
	names, err := s.ListTemplates()
	if err != nil {
		slog.Error("list templates", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return false
	}
	if !slices.Contains(names, name) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown template " + name})
		return false
	}
	if !key.Allows(name) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "API key not allowed to render " + name})
		return false
	}
	return true
}

func (s *Server) postJob(w http.ResponseWriter, r *http.Request) {
	key := callerKey(r)
	body, err := readBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var req jobRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Template == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {template, data?, filename?}"})
		return
	}
	if !s.checkTemplate(w, key, req.Template) {
		return
	}
	if req.Filename != "" && !filenameRe.MatchString(req.Filename) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "filename must be a plain .pdf name"})
		return
	}
	data := []byte(req.Data)
	if len(data) == 0 {
		data = []byte("{}")
	}
	job, err := s.Store.Enqueue(key.ID, req.Template, data, req.Filename, s.MaxAttempts)
	if err != nil {
		slog.Error("enqueue", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	s.Pool.Nudge()
	slog.Info("job queued", "job", job.ID, "template", job.Template, "key", key.Name)
	writeJSON(w, http.StatusAccepted, toJobJSON(job, false))
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	key := callerKey(r)
	job, err := s.Store.GetJob(r.PathValue("id"), key.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown job"})
			return
		}
		slog.Error("get job", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, toJobJSON(job, r.URL.Query().Get("include") == "data"))
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	key := callerKey(r)
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	jobs, err := s.Store.ListJobs(key.ID, q.Get("status"), q.Get("template"), limit)
	if err != nil {
		slog.Error("list jobs", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]jobJSON, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, toJobJSON(j, false))
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

func (s *Server) getJobPDF(w http.ResponseWriter, r *http.Request) {
	key := callerKey(r)
	job, err := s.Store.GetJob(r.PathValue("id"), key.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown job"})
			return
		}
		slog.Error("get job", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if job.Status != store.StatusSucceeded || job.PDFPath == "" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "job is " + job.Status + "; no PDF available", "status": job.Status,
		})
		return
	}
	pdf, err := os.ReadFile(job.PDFPath)
	if err != nil {
		slog.Error("read artifact", "job", job.ID, "path", job.PDFPath, "error", err)
		writeJSON(w, http.StatusGone, map[string]string{"error": "artifact no longer available (retention?)"})
		return
	}
	name := job.Filename
	if name == "" {
		name = job.Template + ".pdf"
	}
	s.writePDF(w, pdf, name, job)
}

// ---- sync render (kept for interactive callers; audited as a job) -----

func (s *Server) syncRender(w http.ResponseWriter, r *http.Request) {
	key := callerKey(r)
	name := r.PathValue("template")
	if !s.checkTemplate(w, key, name) {
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	jobID, err := store.NewJobID()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	start := time.Now()
	res, err := s.Renderer.Render(r.Context(), name, body)
	if err != nil {
		// The failure is part of the audit trail too.
		if _, rerr := s.Store.RecordSyncJob(jobID, key.ID, name, body, "", nil, err.Error()); rerr != nil {
			slog.Error("record failed sync job", "error", rerr)
		}
		var ce *render.CompileError
		switch {
		case errors.Is(err, render.ErrInvalidName):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		case errors.Is(err, os.ErrNotExist):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown template " + name})
		case errors.As(err, &ce):
			// The template/data combination didn't compile: the caller
			// gets typst's diagnostics to fix their input.
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "typst compile failed", "detail": ce.Output,
			})
		case errors.Is(err, context.DeadlineExceeded):
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "compile timed out"})
		default:
			slog.Error("sync render", "template", name, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		}
		return
	}

	artifact, err := s.Pool.StoreArtifact(jobID, name, res)
	if err != nil {
		slog.Error("store sync artifact", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	job, err := s.Store.RecordSyncJob(jobID, key.ID, name, body, downloadName(r, name), artifact, "")
	if err != nil {
		slog.Error("record sync job", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	slog.Info("sync render", "job", job.ID, "template", name, "bytes", len(res.PDF),
		"duration", time.Since(start).Round(time.Millisecond).String(), "key", key.Name)
	s.writePDF(w, res.PDF, downloadName(r, name), job)
}

// writePDF sends PDF bytes with the integrity/provenance headers.
func (s *Server) writePDF(w http.ResponseWriter, pdf []byte, filename string, job *store.Job) {
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="`+filename+`"`)
	w.Header().Set("X-Job-Id", job.ID)
	if job.PDFSha256 != "" {
		w.Header().Set("X-Pdf-Sha256", job.PDFSha256)
	}
	if job.Signature != "" {
		w.Header().Set("X-Pdf-Signature", job.Signature)
	}
	if job.PDFStandard != "" {
		w.Header().Set("X-Pdf-Standard", job.PDFStandard)
	}
	if job.TemplateVersion != "" {
		w.Header().Set("X-Template-Version", job.TemplateVersion)
	}
	w.Write(pdf)
}

// ---- misc endpoints ---------------------------------------------------

func (s *Server) getTemplates(w http.ResponseWriter, r *http.Request) {
	names, err := s.ListTemplates()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	type tpl struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	out := make([]tpl, 0, len(names))
	for _, n := range names {
		out = append(out, tpl{Name: n, Version: worker.TemplateVersion(s.TemplatesDir, n)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

func (s *Server) getSigningKey(w http.ResponseWriter, r *http.Request) {
	if s.Signer == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "signing not configured"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"algorithm": "ed25519", "public_key": s.Signer.PublicKey(),
	})
}

// ---- helpers ----------------------------------------------------------

func readBody(req *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, req.Body, maxBodyBytes))
	if err != nil {
		return nil, errors.New("body too large or unreadable")
	}
	if len(body) == 0 {
		body = []byte("{}")
	}
	if !json.Valid(body) {
		return nil, errors.New("body must be valid JSON")
	}
	return body, nil
}

var filenameRe = regexp.MustCompile(`^[A-Za-z0-9._ -]+\.pdf$`)

// downloadName picks the response filename: an optional ?filename= wins
// if it is a safe .pdf name, otherwise <template>.pdf.
func downloadName(req *http.Request, template string) string {
	if fn := req.URL.Query().Get("filename"); filenameRe.MatchString(fn) {
		return fn
	}
	return template + ".pdf"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
