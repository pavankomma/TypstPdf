package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- Template-name guard (pure, so it can't silently regress) ---------

func TestValidateTemplateName(t *testing.T) {
	valid := []string{"invoice", "contract_note", "a.b-c_1", "UPPER"}
	for _, name := range valid {
		if err := validateTemplateName(name); err != nil {
			t.Errorf("%q should be valid: %v", name, err)
		}
	}
	invalid := []string{
		"",              // empty
		"..",            // bare traversal
		"../evil",       // traversal with separator
		"a..b",          // embedded dot-dot (defense-in-depth)
		"a/b",           // path separator
		`a\b`,           // windows path separator
		"/etc/passwd",   // rootful (the guard canopy got wrong on Windows)
		`C:\evil`,       // windows absolute
		"name with sp",  // whitespace
		"café",          // non-ASCII
	}
	for _, name := range invalid {
		if err := validateTemplateName(name); err == nil {
			t.Errorf("%q should be rejected", name)
		}
	}
}

// ---- Defaults merge ---------------------------------------------------

func mustMerge(t *testing.T, defaults, body string) map[string]any {
	t.Helper()
	out, err := mergeDefaults([]byte(defaults), []byte(body))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("merged output not an object: %v", err)
	}
	return m
}

func TestMergeDefaultsOverlayWins(t *testing.T) {
	m := mustMerge(t, `{"a":"—","b":"—"}`, `{"a":"real"}`)
	if m["a"] != "real" || m["b"] != "—" {
		t.Fatalf("overlay should win, defaults should fill gaps: %v", m)
	}
}

func TestMergeDefaultsNestedObjectsMergeRecursively(t *testing.T) {
	// A caller sending a partial nested object must not wipe out the
	// sibling placeholder keys (a shallow merge would, and the template
	// would hard-error on the missing key).
	m := mustMerge(t,
		`{"seller":{"name":"—","address":[]}}`,
		`{"seller":{"name":"Acme"}}`)
	seller := m["seller"].(map[string]any)
	if seller["name"] != "Acme" {
		t.Fatalf("nested overlay should win: %v", seller)
	}
	if _, ok := seller["address"]; !ok {
		t.Fatalf("nested default should survive: %v", seller)
	}
}

func TestMergeDefaultsSkipsNulls(t *testing.T) {
	// Canopy's null-skip lesson: a JSON null must never clobber a
	// placeholder a template expects to be a string.
	m := mustMerge(t, `{"a":"—"}`, `{"a":null}`)
	if m["a"] != "—" {
		t.Fatalf("null must not overwrite the placeholder: %v", m)
	}
}

func TestMergeDefaultsArraysReplace(t *testing.T) {
	m := mustMerge(t, `{"items":[]}`, `{"items":[{"x":1}]}`)
	if len(m["items"].([]any)) != 1 {
		t.Fatalf("caller's array should replace the default: %v", m)
	}
}

func TestMergeDefaultsNonObjectBodyPassesThrough(t *testing.T) {
	out, err := mergeDefaults([]byte(`{"a":"—"}`), []byte(`[1,2,3]`))
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if string(out) != `[1,2,3]` {
		t.Fatalf("non-object body must pass through unchanged: %s", out)
	}
}

func TestMergeDefaultsRejectsNonObjectDefaults(t *testing.T) {
	if _, err := mergeDefaults([]byte(`[1]`), []byte(`{}`)); err == nil {
		t.Fatal("non-object defaults file must be an error")
	}
}

// ---- Output validation ------------------------------------------------

func TestValidatePDFBytes(t *testing.T) {
	good := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("x"), 200)...)
	if err := validatePDFBytes(good); err != nil {
		t.Errorf("plausible PDF rejected: %v", err)
	}
	if err := validatePDFBytes([]byte("%PDF-")); err == nil {
		t.Error("tiny output should be rejected")
	}
	notPDF := bytes.Repeat([]byte("A"), 200)
	if err := validatePDFBytes(notPDF); err == nil {
		t.Error("output without PDF magic should be rejected")
	}
}

func TestValidatePDFStandard(t *testing.T) {
	for _, ok := range []string{"", "a-2b", "1.7", "ua-1"} {
		if err := ValidatePDFStandard(ok); err != nil {
			t.Errorf("%q should be accepted: %v", ok, err)
		}
	}
	for _, bad := range []string{"a2b", "A-2B", "pdf/a", "banana"} {
		if err := ValidatePDFStandard(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

// ---- Real-engine render tests (canopy's #584 pattern) ------------------
//
// Drive every template in templates/ through the real typst binary, twice:
// with its examples/ payload, and with an EMPTY body. The empty render is
// the guard that matters: typst hard-errors on a missing dictionary key,
// so a template edit that starts reading a key absent from its
// defaults.json fails HERE instead of 500-ing in production.

func repoPath(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{"..", ".."}, parts...)...)
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func newTestRenderer(t *testing.T, pdfStandard string) *Renderer {
	t.Helper()
	if _, err := exec.LookPath("typst"); err != nil {
		t.Skip("typst binary not on PATH; skipping render tests")
	}
	return New("typst", repoPath(t, "templates"), Options{
		FontsDir:    repoPath(t, "fonts"),
		PDFStandard: pdfStandard,
		Concurrency: 2,
		Timeout:     60 * time.Second,
	})
}

func assertIsPDF(t *testing.T, b []byte) {
	t.Helper()
	if len(b) < 500 {
		t.Fatalf("PDF should be substantial, got %d bytes", len(b))
	}
	if !bytes.HasPrefix(b, []byte("%PDF-")) {
		t.Fatal("rendered bytes should start with the PDF magic header")
	}
}

func TestRenderEveryTemplate(t *testing.T) {
	r := newTestRenderer(t, "")
	names, err := r.Templates()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no templates found")
	}
	for _, name := range names {
		t.Run(name+"/example", func(t *testing.T) {
			payload, err := os.ReadFile(repoPath(t, "examples", name+".json"))
			if err != nil {
				t.Fatalf("every template needs an examples/%s.json: %v", name, err)
			}
			res, err := r.Render(context.Background(), name, payload)
			if err != nil {
				t.Fatalf("render with example payload: %v", err)
			}
			assertIsPDF(t, res.PDF)
		})
		t.Run(name+"/empty-body", func(t *testing.T) {
			// Renders on defaults.json placeholders alone. A failure here
			// means the template reads a key its defaults file doesn't
			// supply — fix the defaults, not the caller.
			res, err := r.Render(context.Background(), name, []byte(`{}`))
			if err != nil {
				t.Fatalf("render with empty body (defaults only): %v", err)
			}
			assertIsPDF(t, res.PDF)
		})
	}
}

func TestRenderPartialNestedPayload(t *testing.T) {
	// The deep-merge guarantee end-to-end: a partial nested object
	// (seller with only a name) must render, with placeholders filling
	// the rest.
	r := newTestRenderer(t, "")
	res, err := r.Render(context.Background(), "invoice",
		[]byte(`{"seller":{"name":"Acme Pty Ltd"},"total":"99.00"}`))
	if err != nil {
		t.Fatalf("partial nested payload should render: %v", err)
	}
	assertIsPDF(t, res.PDF)
}

func TestRenderPDFAStandard(t *testing.T) {
	r := newTestRenderer(t, "a-2b")
	payload, err := os.ReadFile(repoPath(t, "examples", "invoice.json"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Render(context.Background(), "invoice", payload)
	if err != nil {
		t.Fatalf("PDF/A-2b render: %v", err)
	}
	assertIsPDF(t, res.PDF)
	if res.ArchivalFallback {
		t.Fatal("invoice should conform to PDF/A-2b without fallback")
	}
}

func TestRenderSourceCompilesInMemoryTemplates(t *testing.T) {
	// The designer preview path: raw source + data, never touching the
	// templates dir.
	r := newTestRenderer(t, "")
	src := []byte(`#let d = json("data.json")
= Hello #d.name`)
	res, err := r.RenderSource(context.Background(), src, []byte(`{"name":"Designer"}`))
	if err != nil {
		t.Fatalf("in-memory source should render: %v", err)
	}
	assertIsPDF(t, res.PDF)

	// A compile error surfaces as CompileError with diagnostics.
	_, err = r.RenderSource(context.Background(), []byte(`#missing_variable`), nil)
	var ce *CompileError
	if !errors.As(err, &ce) || ce.Output == "" {
		t.Fatalf("broken source should be a CompileError with diagnostics, got %v", err)
	}
}

func TestRenderResolvesSharedComponents(t *testing.T) {
	// The staged scratch tree carries components/, so templates (and
	// unsaved designer previews) can import shared partials — here the
	// branded page chrome driven by d.page.
	r := newTestRenderer(t, "")
	src := []byte(`#let d = json("data.json")
#import "components/page.typ": branded
#show: branded.with(d)
= Branded document`)
	data := []byte(`{"page":{"header_left":"ACME Corp","header_right":"Confidential",` +
		`"footer_left":"typstpdf","page_numbers":true}}`)
	res, err := r.RenderSource(context.Background(), src, data)
	if err != nil {
		t.Fatalf("component import should resolve in the scratch tree: %v", err)
	}
	assertIsPDF(t, res.PDF)

	// No page object at all still renders (every key is optional).
	res, err = r.RenderSource(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("branded page must tolerate a missing page object: %v", err)
	}
	assertIsPDF(t, res.PDF)
}

func TestRenderSourceSVGReturnsPerPageMarkup(t *testing.T) {
	r := newTestRenderer(t, "")
	src := []byte(`#set page(paper: "a4")
= Page one
#pagebreak()
= Page two`)
	pages, err := r.RenderSourceSVG(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("svg preview should render: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("expected 2 svg pages, got %d", len(pages))
	}
	for i, p := range pages {
		if !strings.Contains(p, "<svg") {
			t.Fatalf("page %d should be svg markup, got: %.60s", i+1, p)
		}
	}

	_, err = r.RenderSourceSVG(context.Background(), []byte(`#broken_var`), nil)
	var ce *CompileError
	if !errors.As(err, &ce) {
		t.Fatalf("broken svg preview should be a CompileError, got %v", err)
	}
}

func TestRenderUnknownTemplateIsNotFound(t *testing.T) {
	r := newTestRenderer(t, "")
	_, err := r.Render(context.Background(), "no_such_template", []byte(`{}`))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unknown template should map to os.ErrNotExist, got: %v", err)
	}
}

func TestRenderRejectsTraversalNames(t *testing.T) {
	r := newTestRenderer(t, "")
	for _, name := range []string{"..", "../invoice", `..\invoice`, "/etc/passwd"} {
		if _, err := r.Render(context.Background(), name, []byte(`{}`)); err == nil ||
			!strings.Contains(err.Error(), "invalid template name") {
			t.Errorf("%q should be rejected by the name guard, got: %v", name, err)
		}
	}
}

func TestCheckDefaultsCatchesMalformedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.typ"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.defaults.json"), []byte(`[not an object`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := New("typst", dir, Options{})
	if err := r.CheckDefaults(); err == nil {
		t.Fatal("malformed defaults file must fail the startup check")
	}
}
