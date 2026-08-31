// Package render compiles Typst templates into PDFs.
//
// For each request it creates a scratch dir containing the caller's
// data.json and a copy of the chosen template, shells out to
// `typst compile`, and returns the PDF bytes. A semaphore bounds the
// number of concurrent compiles since typst is CPU-heavy.
//
// Hardening (patterned after canopy's notice pipeline):
//
//   - Missing-key resilience: Typst hard-errors on a missing dictionary
//     key, so a caller omitting one field a template touches would fail
//     the whole render. If templates/<name>.defaults.json exists, the
//     request body is deep-merged over it (JSON nulls skipped), so every
//     key the template reads is always present — worst case the PDF
//     shows a placeholder instead of the render failing.
//   - Deterministic fonts: compiles run with --ignore-system-fonts plus
//     an optional pinned --font-path, so the same request renders the
//     same PDF on every host.
//   - Archival PDF/A: an optional --pdf-standard is enforced, falling
//     back LOUDLY to a baseline PDF when the document fails conformance
//     (a document that must ship still ships; the archival gap becomes
//     a log signal instead of a render failure).
//   - Output validation: bytes are checked for the %PDF- magic before
//     being returned.
package render

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// CompileError carries typst's diagnostics for a template/data problem,
// as opposed to an infrastructure failure.
type CompileError struct {
	Template string
	Output   string
}

func (e *CompileError) Error() string {
	return fmt.Sprintf("typst compile %s failed:\n%s", e.Template, e.Output)
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ErrInvalidName marks a rejected template name so the HTTP layer can map
// it to a 400 rather than a generic 500.
var ErrInvalidName = errors.New("invalid template name")

// validateTemplateName rejects any template name that could reach outside
// the templates directory. Allowlist first (letters, digits, dot, dash,
// underscore — no separators, so no absolute paths and no traversal on any
// OS), then an explicit ".." check as defense-in-depth. Kept as a pure
// function so the guard is unit-tested and can't silently regress.
func validateTemplateName(name string) error {
	if name == "" || !nameRe.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("%w %q", ErrInvalidName, name)
	}
	return nil
}

// pdfStandards is the allowlist of --pdf-standard values accepted by
// typst 0.15; validated at startup so a typo fails fast instead of
// failing every render.
var pdfStandards = map[string]bool{
	"1.4": true, "1.5": true, "1.6": true, "1.7": true, "2.0": true,
	"a-1b": true, "a-1a": true, "a-2b": true, "a-2u": true, "a-2a": true,
	"a-3b": true, "a-3u": true, "a-3a": true,
	"a-4": true, "a-4f": true, "a-4e": true, "ua-1": true,
}

// ValidatePDFStandard checks a -pdf-standard flag value ("" = baseline).
func ValidatePDFStandard(std string) error {
	if std != "" && !pdfStandards[std] {
		return fmt.Errorf("unsupported pdf standard %q", std)
	}
	return nil
}

// Result is a successfully rendered, validated PDF.
type Result struct {
	PDF    []byte
	Sha256 [sha256.Size]byte
	// ArchivalFallback is true when a PDF standard was requested but the
	// document failed conformance and the baseline re-render was returned
	// instead (mirrors canopy's #337 loud-fallback: the document still
	// ships; the archival gap is an ops signal, not a render failure).
	ArchivalFallback bool
}

type Renderer struct {
	TypstBin     string        // path to the typst executable
	TemplatesDir string        // directory holding *.typ templates
	FontsDir     string        // pinned fonts dir ("" = typst's embedded fonts only)
	PDFStandard  string        // --pdf-standard to enforce ("" = baseline PDF)
	SystemFonts  bool          // allow host system fonts (non-deterministic; off by default)
	Timeout      time.Duration // per-compile timeout
	sem          chan struct{} // bounds concurrent typst processes
}

type Options struct {
	FontsDir    string
	PDFStandard string
	SystemFonts bool
	Concurrency int
	Timeout     time.Duration
}

func New(typstBin, templatesDir string, opts Options) *Renderer {
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	// The compile runs with cwd inside the scratch dir, so the font path
	// must be absolute to survive the chdir.
	fontsDir := opts.FontsDir
	if fontsDir != "" {
		if abs, err := filepath.Abs(fontsDir); err == nil {
			fontsDir = abs
		}
	}
	return &Renderer{
		TypstBin:     typstBin,
		TemplatesDir: templatesDir,
		FontsDir:     fontsDir,
		PDFStandard:  opts.PDFStandard,
		SystemFonts:  opts.SystemFonts,
		Timeout:      opts.Timeout,
		sem:          make(chan struct{}, opts.Concurrency),
	}
}

// Version reports the typst binary's version, doubling as a health check.
func (r *Renderer) Version(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, r.TypstBin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("typst not runnable at %q: %w", r.TypstBin, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Templates lists the template names (without .typ) available for rendering.
func (r *Renderer) Templates() ([]string, error) {
	entries, err := os.ReadDir(r.TemplatesDir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".typ") {
			names = append(names, strings.TrimSuffix(e.Name(), ".typ"))
		}
	}
	return names, nil
}

// CheckDefaults parses every *.defaults.json in the templates dir so a
// malformed defaults file fails at startup, not on the first render.
func (r *Renderer) CheckDefaults() error {
	entries, err := os.ReadDir(r.TemplatesDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".defaults.json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(r.TemplatesDir, e.Name()))
		if err != nil {
			return err
		}
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("%s: not a JSON object: %w", e.Name(), err)
		}
	}
	return nil
}

// Render compiles the named template against the given JSON document and
// returns the validated PDF. Templates read their input with
// `json("data.json")`; the body is deep-merged over the template's
// defaults.json (if any) first.
func (r *Renderer) Render(ctx context.Context, template string, data []byte) (*Result, error) {
	if err := validateTemplateName(template); err != nil {
		return nil, err
	}
	tpl, err := os.ReadFile(filepath.Join(r.TemplatesDir, template+".typ"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("unknown template %q: %w", template, os.ErrNotExist)
		}
		return nil, err
	}

	data, err = r.applyDefaults(template, data)
	if err != nil {
		return nil, err
	}

	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	dir, err := os.MkdirTemp("", "typstpdf-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	if err := os.WriteFile(filepath.Join(dir, "data.json"), data, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "main.typ"), tpl, 0o644); err != nil {
		return nil, err
	}

	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}

	pdf, err := r.compile(ctx, dir, template, r.PDFStandard)
	fallback := false
	if err != nil {
		var ce *CompileError
		// Loud archival fallback: with a PDF standard requested, a compile
		// error may be a conformance violation rather than a template/data
		// problem. Re-render baseline once; if that succeeds, the document
		// still ships and the gap is logged. If the baseline render fails
		// too, the original (data) error is the one that matters.
		if r.PDFStandard != "" && errors.As(err, &ce) {
			if pdf2, err2 := r.compile(ctx, dir, template, ""); err2 == nil {
				log.Printf(
					"ARCHIVAL FALLBACK: template %s failed PDF/%s conformance; served baseline PDF — re-render locally for diagnostics",
					template, r.PDFStandard,
				)
				pdf, err, fallback = pdf2, nil, true
			}
		}
		if err != nil {
			return nil, err
		}
	}

	if err := validatePDFBytes(pdf); err != nil {
		return nil, fmt.Errorf("template %s: %w", template, err)
	}
	return &Result{PDF: pdf, Sha256: sha256.Sum256(pdf), ArchivalFallback: fallback}, nil
}

// compile runs one `typst compile` in the prepared scratch dir.
func (r *Renderer) compile(ctx context.Context, dir, template, pdfStandard string) ([]byte, error) {
	args := []string{"compile", "main.typ", "out.pdf"}
	if !r.SystemFonts {
		// Determinism: the same request must render the same PDF on every
		// host. Typst's embedded fonts (Libertinus, New Computer Modern,
		// DejaVu Sans Mono) remain available.
		args = append(args, "--ignore-system-fonts")
	}
	if r.FontsDir != "" {
		args = append(args, "--font-path", r.FontsDir)
	}
	if pdfStandard != "" {
		args = append(args, "--pdf-standard", pdfStandard)
	}
	cmd := exec.CommandContext(ctx, r.TypstBin, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("typst compile %s: %w", template, ctx.Err())
		}
		return nil, &CompileError{Template: template, Output: string(out)}
	}
	return os.ReadFile(filepath.Join(dir, "out.pdf"))
}

// applyDefaults deep-merges the request body over the template's
// defaults.json, if one exists. With no defaults file the body passes
// through untouched.
func (r *Renderer) applyDefaults(template string, body []byte) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(r.TemplatesDir, template+".defaults.json"))
	if errors.Is(err, os.ErrNotExist) {
		return body, nil
	}
	if err != nil {
		return nil, err
	}
	merged, err := mergeDefaults(raw, body)
	if err != nil {
		return nil, fmt.Errorf("template %s defaults: %w", template, err)
	}
	return merged, nil
}

// mergeDefaults overlays the request JSON onto the defaults JSON.
// Objects merge recursively; scalars and arrays from the request replace
// the default; JSON nulls in the request are SKIPPED so a null can never
// clobber a placeholder a template expects to be a string (canopy's
// null-skip lesson). A non-object request body passes through unchanged.
func mergeDefaults(defaults, body []byte) ([]byte, error) {
	var def map[string]any
	if err := json.Unmarshal(defaults, &def); err != nil {
		return nil, fmt.Errorf("defaults must be a JSON object: %w", err)
	}
	var over map[string]any
	if err := json.Unmarshal(body, &over); err != nil {
		// Valid JSON but not an object (array, scalar): the template gets
		// exactly what the caller sent.
		return body, nil
	}
	deepMerge(def, over)
	return json.Marshal(def)
}

func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if v == nil {
			continue // null-skip: never overwrite a placeholder with null
		}
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				deepMerge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

// validatePDFBytes asserts the render output is a plausible PDF (magic
// header + non-trivial size) before it is returned to any caller.
func validatePDFBytes(b []byte) error {
	if len(b) < 100 {
		return fmt.Errorf("render produced implausibly small output (%d bytes)", len(b))
	}
	if !strings.HasPrefix(string(b[:8]), "%PDF-") {
		return errors.New("render output missing %PDF- magic header")
	}
	return nil
}
