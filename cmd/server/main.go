// Command server is an HTTP PDF-generation service powered by Typst.
//
//	server -addr :8080 -templates templates -concurrency 8
//
// API:
//
//	GET  /healthz            → {"status":"ok","typst":"typst 0.15.1"}
//	GET  /templates          → {"templates":["invoice","letter",...]}
//	POST /render/{template}  → JSON body in, application/pdf out
//
// Templates live in the templates dir as self-contained .typ files that
// read their input with `json("data.json")`. An optional
// <template>.defaults.json beside a template supplies placeholder values
// for every key it reads, so partial request bodies render placeholders
// instead of failing the compile.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"time"

	"github.com/pavankomma/TypstPdf/internal/render"
)

const maxBodyBytes = 20 << 20 // 20 MiB of JSON input per request

func main() {
	log.SetFlags(log.Ltime)
	addr := flag.String("addr", ":8080", "listen address")
	templates := flag.String("templates", "templates", "directory of .typ templates")
	typstBin := flag.String("typst", "typst", "path to the typst executable")
	fonts := flag.String("fonts", "fonts", "pinned fonts directory (empty = typst's embedded fonts only)")
	systemFonts := flag.Bool("system-fonts", false, "allow host system fonts (breaks render determinism)")
	pdfStandard := flag.String("pdf-standard", "", "PDF standard to enforce, e.g. a-2b (empty = baseline PDF)")
	concurrency := flag.Int("concurrency", runtime.NumCPU(), "max concurrent typst compiles")
	timeout := flag.Duration("timeout", 30*time.Second, "per-compile timeout")
	flag.Parse()

	if err := render.ValidatePDFStandard(*pdfStandard); err != nil {
		log.Fatalf("startup check failed: %v", err)
	}
	// A missing fonts dir is fine (embedded fonts only) unless one was
	// explicitly requested; the default just quietly doesn't apply.
	if *fonts != "" {
		if _, err := os.Stat(*fonts); err != nil {
			log.Printf("fonts dir %q not found; using typst's embedded fonts only", *fonts)
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
		log.Fatalf("startup check failed: %v (install Typst or pass -typst)", err)
	}
	if _, err := r.Templates(); err != nil {
		log.Fatalf("startup check failed: cannot read templates dir: %v", err)
	}
	if err := r.CheckDefaults(); err != nil {
		log.Fatalf("startup check failed: %v", err)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "typst": version})
	})

	mux.HandleFunc("GET /templates", func(w http.ResponseWriter, req *http.Request) {
		names, err := r.Templates()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"templates": names})
	})

	mux.HandleFunc("POST /render/{template}", func(w http.ResponseWriter, req *http.Request) {
		name := req.PathValue("template")
		body, err := readBody(req)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		start := time.Now()
		res, err := r.Render(req.Context(), name, body)
		if err != nil {
			var ce *render.CompileError
			switch {
			case errors.Is(err, render.ErrInvalidName):
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			case errors.Is(err, os.ErrNotExist):
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown template " + name})
			case errors.As(err, &ce):
				// The template/data combination didn't compile: the
				// caller gets typst's diagnostics to fix their input.
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
					"error": "typst compile failed", "detail": ce.Output,
				})
			case errors.Is(err, context.DeadlineExceeded):
				writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "compile timed out"})
			default:
				log.Printf("render %s: %v", name, err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			}
			return
		}
		log.Printf("rendered %s: %d bytes in %s", name, len(res.PDF), time.Since(start).Round(time.Millisecond))

		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `inline; filename="`+downloadName(req, name)+`"`)
		w.Header().Set("X-Pdf-Sha256", hex.EncodeToString(res.Sha256[:]))
		if *pdfStandard != "" {
			std := *pdfStandard
			if res.ArchivalFallback {
				std = "baseline-fallback"
			}
			w.Header().Set("X-Pdf-Standard", std)
		}
		w.Write(res.PDF)
	})

	log.Printf("typst-pdf service on %s | %s | templates=%s fonts=%s pdf-standard=%s concurrency=%d",
		*addr, version, *templates, orNone(*fonts), orNone(*pdfStandard), *concurrency)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

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
