// Command server is an HTTP PDF-generation service powered by Typst,
// with a SQLite-backed job queue, API-key auth, Ed25519 artifact
// signatures, and Prometheus metrics.
//
//	server                        # serve (flags below)
//	server keys create <name> [-templates invoice,letter]
//	server keys list
//	server keys disable <name>
//
// API (bearer auth except /healthz and /metrics):
//
//	POST /v1/jobs                → queue a render, 202 {id,...}
//	GET  /v1/jobs/{id}           → job status + provenance
//	GET  /v1/jobs/{id}/pdf       → the artifact
//	GET  /v1/jobs                → caller's jobs
//	POST /render/{template}      → synchronous render (audited as a job)
//	GET  /templates              → names + content-hash versions
//	GET  /v1/signing-key         → Ed25519 public key
//	GET  /healthz, GET /metrics
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pavankomma/TypstPdf/internal/api"
	"github.com/pavankomma/TypstPdf/internal/render"
	"github.com/pavankomma/TypstPdf/internal/sign"
	"github.com/pavankomma/TypstPdf/internal/store"
	"github.com/pavankomma/TypstPdf/internal/worker"
	designerui "github.com/pavankomma/TypstPdf/web/designer"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if len(os.Args) > 1 && os.Args[1] == "keys" {
		if err := runKeys(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if err := runServe(os.Args[1:]); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// ---- keys subcommand --------------------------------------------------

func runKeys(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: server keys <create|list|disable> ...")
	}
	verb, rest := args[0], args[1:]
	fs := flag.NewFlagSet("keys "+verb, flag.ExitOnError)
	db := fs.String("db", "data/typstpdf.db", "path to the SQLite database")
	templates := fs.String("templates", "", "comma-separated template allowlist (create; empty = all)")
	// Accept "keys create NAME -templates ..." — name first, flags after.
	var name string
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		name, rest = rest[0], rest[1:]
	}
	fs.Parse(rest)

	if err := os.MkdirAll(filepath.Dir(*db), 0o755); err != nil {
		return err
	}
	s, err := store.Open(*db)
	if err != nil {
		return err
	}
	defer s.Close()

	switch verb {
	case "create":
		if name == "" {
			return errors.New("usage: server keys create <name> [-templates a,b]")
		}
		var allow []string
		if *templates != "" {
			allow = strings.Split(*templates, ",")
		}
		plain, err := s.CreateKey(name, allow)
		if err != nil {
			return err
		}
		fmt.Printf("API key %q created. Shown ONCE — store it now:\n%s\n", name, plain)
		return nil
	case "list":
		keys, err := s.ListKeys()
		if err != nil {
			return err
		}
		for _, k := range keys {
			state := "enabled"
			if k.Disabled {
				state = "disabled"
			}
			fmt.Printf("%-4d %-20s %-9s templates=%s created=%s\n",
				k.ID, k.Name, state, k.Templates, k.CreatedAt.Format(time.RFC3339))
		}
		return nil
	case "disable":
		if name == "" {
			return errors.New("usage: server keys disable <name>")
		}
		if err := s.DisableKey(name); err != nil {
			return err
		}
		fmt.Printf("key %q disabled\n", name)
		return nil
	default:
		return fmt.Errorf("unknown keys verb %q", verb)
	}
}

// ---- serve ------------------------------------------------------------

func runServe(args []string) error {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	templates := fs.String("templates", "templates", "directory of .typ templates")
	typstBin := fs.String("typst", "typst", "path to the typst executable")
	fonts := fs.String("fonts", "fonts", "pinned fonts directory (empty = typst's embedded fonts only)")
	systemFonts := fs.Bool("system-fonts", false, "allow host system fonts (breaks render determinism)")
	pdfStandard := fs.String("pdf-standard", "", "PDF standard to enforce, e.g. a-2b (empty = baseline PDF)")
	concurrency := fs.Int("concurrency", runtime.NumCPU(), "max concurrent typst compiles / queue workers")
	timeout := fs.Duration("timeout", 30*time.Second, "per-compile timeout")
	db := fs.String("db", "data/typstpdf.db", "path to the SQLite database")
	artifacts := fs.String("artifacts", "data/artifacts", "directory for rendered PDFs")
	signingKey := fs.String("signing-key", "data/signing.key", "Ed25519 seed file (created on first boot; empty = signing off)")
	retention := fs.Duration("retention", 720*time.Hour, "delete finished jobs + artifacts older than this (0 = keep forever)")
	maxAttempts := fs.Int("max-attempts", 3, "render attempts per queued job")
	noAuth := fs.Bool("no-auth", false, "DEV ONLY: skip API-key auth; requests act as the 'anonymous' key")
	designer := fs.Bool("designer", false, "enable the template designer UI + API (trusted environments only)")
	examples := fs.String("examples", "examples", "directory of example payloads (used by the designer)")
	tlsCert := fs.String("tls-cert", "", "TLS certificate file (with -tls-key enables HTTPS)")
	tlsKey := fs.String("tls-key", "", "TLS private key file")
	fs.Parse(args)

	if err := render.ValidatePDFStandard(*pdfStandard); err != nil {
		return fmt.Errorf("startup check: %w", err)
	}
	if *fonts != "" {
		if _, err := os.Stat(*fonts); err != nil {
			slog.Warn("fonts dir not found; using typst's embedded fonts only", "dir", *fonts)
			*fonts = ""
		}
	}

	r := render.New(*typstBin, *templates, render.Options{
		FontsDir:    *fonts,
		PDFStandard: *pdfStandard,
		SystemFonts: *systemFonts,
		Concurrency: *concurrency,
		Timeout:     *timeout,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	version, err := r.Version(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("startup check: %w (install Typst or pass -typst)", err)
	}
	if _, err := r.Templates(); err != nil {
		return fmt.Errorf("startup check: cannot read templates dir: %w", err)
	}
	if err := r.CheckDefaults(); err != nil {
		return fmt.Errorf("startup check: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(*db), 0o755); err != nil {
		return err
	}
	st, err := store.Open(*db)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	// Requeue jobs stranded in 'running' by a previous crash.
	if n, err := st.RecoverRunning(); err != nil {
		return fmt.Errorf("crash recovery: %w", err)
	} else if n > 0 {
		slog.Info("requeued jobs stranded by a previous shutdown", "count", n)
	}

	var signer *sign.Signer
	if *signingKey != "" {
		signer, err = sign.Load(*signingKey)
		if err != nil {
			return fmt.Errorf("signing key: %w", err)
		}
		slog.Info("artifact signing enabled", "public_key", signer.PublicKey())
	}

	var anonKey *store.APIKey
	if *noAuth {
		id, err := st.EnsureKey("anonymous")
		if err != nil {
			return err
		}
		anonKey = &store.APIKey{ID: id, Name: "anonymous", Templates: "*"}
		slog.Warn("running with -no-auth: every request acts as the 'anonymous' key")
	}

	pool := &worker.Pool{
		Store:        st,
		Renderer:     r,
		Signer:       signer,
		ArtifactsDir: *artifacts,
		TemplatesDir: *templates,
		PDFStandard:  *pdfStandard,
		Workers:      *concurrency,
	}

	srv := &api.Server{
		Store:         st,
		Renderer:      r,
		Pool:          pool,
		Signer:        signer,
		TemplatesDir:  *templates,
		ListTemplates: r.Templates,
		TypstVersion:  version,
		MaxAttempts:   *maxAttempts,
		NoAuth:        *noAuth,
		AnonKey:       anonKey,
	}
	if *designer {
		ui, err := designerui.Dist()
		if err != nil {
			return fmt.Errorf("designer UI assets: %w", err)
		}
		srv.Designer = true
		srv.SourceRenderer = r
		srv.ExamplesDir = *examples
		srv.DesignerUI = ui
		slog.Info("template designer enabled", "url", "/designer/")
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	poolDone := make(chan struct{})
	go func() { pool.Run(rootCtx); close(poolDone) }()
	if *retention > 0 {
		go janitor(rootCtx, st, *retention)
	}

	httpServer := &http.Server{Addr: *addr, Handler: srv.Handler()}
	go func() {
		<-rootCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdownCtx)
	}()

	scheme := "http"
	serve := func() error { return httpServer.ListenAndServe() }
	if *tlsCert != "" && *tlsKey != "" {
		scheme = "https"
		serve = func() error { return httpServer.ListenAndServeTLS(*tlsCert, *tlsKey) }
	}
	slog.Info("typst-pdf service up",
		"addr", *addr, "scheme", scheme, "typst", version,
		"templates", *templates, "fonts", orNone(*fonts), "pdf_standard", orNone(*pdfStandard),
		"db", *db, "artifacts", *artifacts, "workers", *concurrency,
		"retention", retention.String(), "auth", !*noAuth, "signing", signer != nil,
	)
	if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-poolDone // let in-flight renders finish
	slog.Info("shutdown complete")
	return nil
}

// janitor deletes finished jobs (and their artifact files) past the
// retention window, hourly.
func janitor(ctx context.Context, st *store.Store, retention time.Duration) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		paths, err := st.PurgeBefore(time.Now().Add(-retention))
		if err != nil {
			slog.Error("retention purge", "error", err)
		} else if len(paths) > 0 {
			removed := 0
			for _, p := range paths {
				if err := os.Remove(p); err == nil || errors.Is(err, os.ErrNotExist) {
					removed++
				} else {
					slog.Warn("remove expired artifact", "path", p, "error", err)
				}
			}
			slog.Info("retention purge", "jobs", len(paths), "artifacts_removed", removed)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
