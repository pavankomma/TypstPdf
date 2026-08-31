// Package worker drains the SQLite job queue: claim → render → write
// artifact (atomic temp+rename) → sign → complete, with failures going
// back through the store's retry/backoff machinery.
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/pavankomma/TypstPdf/internal/render"
	"github.com/pavankomma/TypstPdf/internal/sign"
	"github.com/pavankomma/TypstPdf/internal/store"
)

// Renderer is the slice of *render.Renderer the pool needs; faked in
// tests so worker and API tests run without a typst binary.
type Renderer interface {
	Render(ctx context.Context, template string, data []byte) (*render.Result, error)
}

var (
	jobsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "typstpdf_jobs_total",
		Help: "Render jobs by terminal outcome (requeued retries count once per attempt).",
	}, []string{"status"})
	renderDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "typstpdf_render_duration_seconds",
		Help:    "Wall time of typst renders.",
		Buckets: prometheus.DefBuckets,
	}, []string{"template"})
	archivalFallbacks = promauto.NewCounter(prometheus.CounterOpts{
		Name: "typstpdf_archival_fallback_total",
		Help: "Documents that failed PDF/A conformance and shipped as baseline PDF.",
	})
	// QueueDepth is registered here and polled by the pool.
	queueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "typstpdf_queue_depth",
		Help: "Jobs queued or running.",
	})
)

type Pool struct {
	Store        *store.Store
	Renderer     Renderer
	Signer       *sign.Signer
	ArtifactsDir string
	TemplatesDir string
	PDFStandard  string
	Workers      int

	nudge chan struct{}
	once  sync.Once
}

// Nudge wakes an idle worker after an enqueue; a 1s ticker is the
// fallback so a missed nudge only delays, never strands, a job.
func (p *Pool) Nudge() {
	p.init()
	select {
	case p.nudge <- struct{}{}:
	default:
	}
}

func (p *Pool) init() {
	p.once.Do(func() { p.nudge = make(chan struct{}, 64) })
}

// Run starts the worker goroutines and blocks until ctx is done and all
// workers have drained their in-flight job.
func (p *Pool) Run(ctx context.Context) {
	p.init()
	n := p.Workers
	if n < 1 {
		n = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.loop(ctx)
		}()
	}
	// Metrics poller for queue depth.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			if d, err := p.Store.QueueDepth(); err == nil {
				queueDepth.Set(float64(d))
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	wg.Wait()
}

func (p *Pool) loop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		job, err := p.Store.Claim(time.Now())
		if err != nil {
			slog.Error("claim failed", "error", err)
		} else if job != nil {
			p.process(ctx, job)
			continue // drain eagerly while work remains
		}
		select {
		case <-ctx.Done():
			return
		case <-p.nudge:
		case <-ticker.C:
		}
	}
}

func (p *Pool) process(ctx context.Context, job *store.Job) {
	log := slog.With("job", job.ID, "template", job.Template, "attempt", job.Attempts)
	start := time.Now()
	res, err := p.Renderer.Render(ctx, job.Template, job.Data)
	renderDuration.WithLabelValues(job.Template).Observe(time.Since(start).Seconds())
	if err != nil {
		status, ferr := p.Store.Fail(job.ID, err.Error())
		if ferr != nil {
			log.Error("record failure", "error", ferr, "render_error", err)
			return
		}
		jobsTotal.WithLabelValues(status).Inc()
		log.Warn("render failed", "outcome", status, "error", err)
		return
	}

	artifact, err := p.StoreArtifact(job.ID, job.Template, res)
	if err != nil {
		status, ferr := p.Store.Fail(job.ID, err.Error())
		if ferr != nil {
			log.Error("record artifact failure", "error", ferr)
			return
		}
		jobsTotal.WithLabelValues(status).Inc()
		log.Error("artifact write failed", "outcome", status, "error", err)
		return
	}

	if err := p.Store.Complete(job.ID, *artifact); err != nil {
		log.Error("complete job", "error", err)
		return
	}
	jobsTotal.WithLabelValues(store.StatusSucceeded).Inc()
	log.Info("job rendered",
		"bytes", artifact.PDFBytes,
		"version", artifact.TemplateVersion,
		"duration", time.Since(start).Round(time.Millisecond).String(),
		"archival_fallback", artifact.ArchivalFallback,
	)
}

// StoreArtifact writes the PDF atomically under the artifacts dir, signs
// it, and assembles the Artifact facts for the job row. Exported so the
// sync /render path stores its artifact identically.
func (p *Pool) StoreArtifact(jobID, template string, res *render.Result) (*store.Artifact, error) {
	if res.ArchivalFallback {
		archivalFallbacks.Inc()
	}
	if err := os.MkdirAll(p.ArtifactsDir, 0o755); err != nil {
		return nil, err
	}
	final := filepath.Join(p.ArtifactsDir, jobID+".pdf")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, res.PDF, 0o644); err != nil {
		return nil, fmt.Errorf("write artifact: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("finalize artifact: %w", err)
	}
	a := &store.Artifact{
		TemplateVersion:  TemplateVersion(p.TemplatesDir, template),
		PDFPath:          final,
		PDFSha256:        hex.EncodeToString(res.Sha256[:]),
		PDFBytes:         int64(len(res.PDF)),
		PDFStandard:      p.PDFStandard,
		ArchivalFallback: res.ArchivalFallback,
	}
	if a.ArchivalFallback {
		a.PDFStandard = "baseline-fallback"
	}
	if p.Signer != nil {
		a.Signature = p.Signer.Sign(res.PDF)
	}
	return a, nil
}

// TemplateVersion identifies the exact template content a document was
// rendered from: sha256 over the .typ source, its defaults.json (if any),
// and every shared partial under components/ (an edit there changes the
// rendering of any template importing it), truncated to 12 hex chars.
// "Which template version produced this document" is the first question
// in any dispute; this answers it.
func TemplateVersion(templatesDir, name string) string {
	h := sha256.New()
	src, err := os.ReadFile(filepath.Join(templatesDir, name+".typ"))
	if err != nil {
		return ""
	}
	h.Write(src)
	if def, err := os.ReadFile(filepath.Join(templatesDir, name+".defaults.json")); err == nil {
		h.Write(def)
	} else if !errors.Is(err, os.ErrNotExist) {
		return ""
	}
	// Shared partials, in stable (sorted) order. No components dir is fine.
	componentsDir := filepath.Join(templatesDir, "components")
	if entries, err := os.ReadDir(componentsDir); err == nil {
		for _, e := range entries { // ReadDir returns sorted entries
			if e.IsDir() {
				continue
			}
			if b, err := os.ReadFile(filepath.Join(componentsDir, e.Name())); err == nil {
				h.Write([]byte(e.Name()))
				h.Write(b)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}
