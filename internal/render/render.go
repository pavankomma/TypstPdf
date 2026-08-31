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
	"slices"
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
// ValidateTemplateName exposes the guard for the designer's save path.
func ValidateTemplateName(name string) error { return validateTemplateName(name) }

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
	if _, err := os.Stat(filepath.Join(r.TemplatesDir, template+".typ")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("unknown template %q: %w", template, os.ErrNotExist)
		}
		return nil, err
	}

	data, err := r.applyDefaults(template, data)
	if err != nil {
		return nil, err
	}

	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	dir, err := r.stageScratch(data)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}

	pdf, err := r.compile(ctx, dir, template+".typ", template, r.PDFStandard)
	fallback := false
	if err != nil {
		var ce *CompileError
		// Loud archival fallback: with a PDF standard requested, a compile
		// error may be a conformance violation rather than a template/data
		// problem. Re-render baseline once; if that succeeds, the document
		// still ships and the gap is logged. If the baseline render fails
		// too, the original (data) error is the one that matters.
		if r.PDFStandard != "" && errors.As(err, &ce) {
			if pdf2, err2 := r.compile(ctx, dir, template+".typ", template, ""); err2 == nil {
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

// RenderSource compiles an in-memory template source (not a file from the
// templates dir) against the given JSON document — the template designer's
// preview path, so edits render before they are saved. It runs through the
// same scratch-dir pipeline, fonts, PDF standard, and output validation as
// Render. data is used verbatim: the caller applies any defaults merge.
func (r *Renderer) RenderSource(ctx context.Context, source, data []byte) (*Result, error) {
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	dir, err := r.stageScratch(data)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	if err := os.WriteFile(filepath.Join(dir, previewMain), source, 0o644); err != nil {
		return nil, err
	}

	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	pdf, err := r.compile(ctx, dir, previewMain, "(designer preview)", r.PDFStandard)
	fallback := false
	if err != nil {
		var ce *CompileError
		if r.PDFStandard != "" && errors.As(err, &ce) {
			if pdf2, err2 := r.compile(ctx, dir, previewMain, "(designer preview)", ""); err2 == nil {
				pdf, err, fallback = pdf2, nil, true
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if err := validatePDFBytes(pdf); err != nil {
		return nil, err
	}
	return &Result{PDF: pdf, Sha256: sha256.Sum256(pdf), ArchivalFallback: fallback}, nil
}

// RenderSourceSVG compiles an in-memory source to per-page SVGs — the
// designer's lightweight inline preview (no PDF viewer chrome). Typst's
// SVG export outlines text to paths, so the pages are self-contained.
// Page files use the zero-padded {0p} placeholder so lexical order is
// page order.
func (r *Renderer) RenderSourceSVG(ctx context.Context, source, data []byte) ([]string, error) {
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	dir, err := r.stageScratch(data)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	if err := os.WriteFile(filepath.Join(dir, previewMain), source, 0o644); err != nil {
		return nil, err
	}

	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	args := []string{"compile", previewMain, "page-{0p}.svg", "--format", "svg"}
	if !r.SystemFonts {
		args = append(args, "--ignore-system-fonts")
	}
	if r.FontsDir != "" {
		args = append(args, "--font-path", r.FontsDir)
	}
	cmd := exec.CommandContext(ctx, r.TypstBin, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("typst svg preview: %w", ctx.Err())
		}
		return nil, &CompileError{Template: "(designer preview)", Output: scrubScratchDir(string(out), dir)}
	}

	files, err := filepath.Glob(filepath.Join(dir, "page-*.svg"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("svg preview produced no pages")
	}
	slices.Sort(files) // zero-padded page numbers → lexical order is page order
	pages := make([]string, 0, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		pages = append(pages, string(b))
	}
	return pages, nil
}

// previewMain is the scratch filename for the designer's unsaved source.
// It sits beside the copied templates, so `#import "components/..."`
// resolves for previews exactly as it does for saved templates.
const previewMain = "__preview__.typ"

// stageScratch builds the per-render scratch dir: a copy of the templates
// tree — so templates can `#import "components/..."` shared partials
// (letterheads, footers, tokens) while renders stay isolated — plus the
// request's data.json at the root.
func (r *Renderer) stageScratch(data []byte) (string, error) {
	dir, err := os.MkdirTemp("", "typstpdf-")
	if err != nil {
		return "", err
	}
	if err := copyTree(r.TemplatesDir, dir); err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("stage templates: %w", err)
	}
	if len(data) == 0 {
		data = []byte("{}")
	}
	if err := os.WriteFile(filepath.Join(dir, "data.json"), data, 0o644); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// copyTree copies the templates tree (files + subdirs) into dst.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

// compile runs one `typst compile` in the prepared scratch dir. mainFile
// is the entry .typ within the scratch tree; template names the render in
// errors.
func (r *Renderer) compile(ctx context.Context, dir, mainFile, template, pdfStandard string) ([]byte, error) {
	args := []string{"compile", mainFile, "out.pdf"}
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
		return nil, &CompileError{Template: template, Output: scrubScratchDir(string(out), dir)}
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

// MergeDefaults exposes the defaults merge for callers that hold the
// defaults themselves (the template designer previews unsaved defaults).
func MergeDefaults(defaults, body []byte) ([]byte, error) {
	return mergeDefaults(defaults, body)
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

// scrubScratchDir removes the per-request temp dir from typst diagnostics
// so callers see "main.typ:3:1", not the host's temp path (noise in the
// designer UI, and a needless environment leak in API responses). Windows
// paths may carry the \\?\ long-path prefix.
func scrubScratchDir(output, dir string) string {
	sep := string(os.PathSeparator)
	output = strings.ReplaceAll(output, `\\?\`+dir+sep, "")
	return strings.ReplaceAll(output, dir+sep, "")
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
