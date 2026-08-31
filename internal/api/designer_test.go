package api

import (
	"context"
	"crypto/sha256"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pavankomma/TypstPdf/internal/render"
	"github.com/pavankomma/TypstPdf/internal/store"
	"github.com/pavankomma/TypstPdf/internal/worker"
)

// fakeSourceRenderer compiles successfully unless the source contains
// "BROKEN".
type fakeSourceRenderer struct{}

func (fakeSourceRenderer) RenderSource(_ context.Context, source, _ []byte) (*render.Result, error) {
	if strings.Contains(string(source), "BROKEN") {
		return nil, &render.CompileError{Template: "(designer preview)", Output: "unknown variable: broken"}
	}
	pdf := append([]byte("%PDF-1.7\npreview"), make([]byte, 600)...)
	return &render.Result{PDF: pdf, Sha256: sha256.Sum256(pdf)}, nil
}

func (fakeSourceRenderer) RenderSourceSVG(_ context.Context, source, _ []byte) ([]string, error) {
	if strings.Contains(string(source), "BROKEN") {
		return nil, &render.CompileError{Template: "(designer preview)", Output: "unknown variable: broken"}
	}
	return []string{"<svg>page 1</svg>", "<svg>page 2</svg>"}, nil
}

type designerHarness struct {
	*harness
	templatesDir string
	examplesDir  string
}

func newDesignerHarness(t *testing.T) *designerHarness {
	t.Helper()
	dir := t.TempDir()
	templatesDir := filepath.Join(dir, "templates")
	examplesDir := filepath.Join(dir, "examples")
	for _, d := range []string{templatesDir, examplesDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Seed one existing template with defaults + example.
	os.WriteFile(filepath.Join(templatesDir, "letter.typ"), []byte(`#let d = json("data.json")`), 0o644)
	os.WriteFile(filepath.Join(templatesDir, "letter.defaults.json"), []byte(`{"a":"—"}`), 0o644)
	os.WriteFile(filepath.Join(examplesDir, "letter.json"), []byte(`{"a":"hi"}`), 0o644)

	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	plain, err := s.CreateKey("ci", nil)
	if err != nil {
		t.Fatal(err)
	}
	pool := &worker.Pool{Store: s, Renderer: fakeRenderer{},
		ArtifactsDir: filepath.Join(dir, "artifacts"), TemplatesDir: templatesDir}
	srv := &Server{
		Store: s, Renderer: fakeRenderer{}, Pool: pool,
		TemplatesDir: templatesDir,
		ListTemplates: func() ([]string, error) {
			entries, err := os.ReadDir(templatesDir)
			if err != nil {
				return nil, err
			}
			var names []string
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".typ") {
					names = append(names, strings.TrimSuffix(e.Name(), ".typ"))
				}
			}
			return names, nil
		},
		MaxAttempts:    3,
		Designer:       true,
		SourceRenderer: fakeSourceRenderer{},
		ExamplesDir:    examplesDir,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &designerHarness{
		harness:      &harness{ts: ts, store: s, pool: pool, key: plain},
		templatesDir: templatesDir,
		examplesDir:  examplesDir,
	}
}

func TestDesignerDisabledByDefault(t *testing.T) {
	h := newHarness(t) // Designer: false
	if resp := h.do(t, "GET", "/v1/designer/templates", "", h.key); resp.StatusCode != 404 {
		t.Fatalf("designer routes must not exist without the flag, got %d", resp.StatusCode)
	}
}

func TestDesignerListAndGet(t *testing.T) {
	h := newDesignerHarness(t)
	var list struct {
		Templates []designerTemplate `json:"templates"`
	}
	resp := h.do(t, "GET", "/v1/designer/templates", "", h.key)
	if resp.StatusCode != 200 {
		t.Fatalf("list should be 200, got %d", resp.StatusCode)
	}
	list = decode[struct {
		Templates []designerTemplate `json:"templates"`
	}](t, resp)
	if len(list.Templates) != 1 || list.Templates[0].Name != "letter" ||
		!list.Templates[0].HasDefaults || !list.Templates[0].HasExample {
		t.Fatalf("unexpected listing: %+v", list.Templates)
	}

	doc := decode[designerDoc](t, h.do(t, "GET", "/v1/designer/templates/letter", "", h.key))
	if !strings.Contains(doc.Source, "data.json") || doc.Defaults == "" || doc.Example == "" || doc.Version == "" {
		t.Fatalf("unexpected doc: %+v", doc)
	}
}

func TestDesignerSaveValidatesBeforeWriting(t *testing.T) {
	h := newDesignerHarness(t)

	// A broken template is rejected with diagnostics and nothing is written.
	resp := h.do(t, "PUT", "/v1/designer/templates/newdoc",
		`{"source":"BROKEN template"}`, h.key)
	if resp.StatusCode != 422 {
		t.Fatalf("broken template should be 422, got %d", resp.StatusCode)
	}
	body := decode[map[string]string](t, resp)
	if !strings.Contains(body["detail"], "unknown variable") {
		t.Fatalf("save rejection should carry diagnostics: %v", body)
	}
	if fileExists(filepath.Join(h.templatesDir, "newdoc.typ")) {
		t.Fatal("rejected template must not be written")
	}

	// A valid template is written along with defaults and example.
	resp = h.do(t, "PUT", "/v1/designer/templates/newdoc",
		`{"source":"#let d = json(\"data.json\")","defaults":"{\"x\":\"—\"}","example":"{\"x\":\"1\"}"}`, h.key)
	if resp.StatusCode != 200 {
		t.Fatalf("valid save should be 200, got %d", resp.StatusCode)
	}
	saved := decode[map[string]string](t, resp)
	if len(saved["version"]) != 12 {
		t.Fatalf("save should return the new content version: %v", saved)
	}
	for _, p := range []string{
		filepath.Join(h.templatesDir, "newdoc.typ"),
		filepath.Join(h.templatesDir, "newdoc.defaults.json"),
		filepath.Join(h.examplesDir, "newdoc.json"),
	} {
		if !fileExists(p) {
			t.Fatalf("expected %s to be written", p)
		}
	}

	// Malformed defaults JSON is a 400.
	resp = h.do(t, "PUT", "/v1/designer/templates/newdoc",
		`{"source":"ok","defaults":"[broken"}`, h.key)
	if resp.StatusCode != 400 {
		t.Fatalf("malformed defaults should be 400, got %d", resp.StatusCode)
	}
}

func TestDesignerSaveRemovesEmptiedSidecars(t *testing.T) {
	h := newDesignerHarness(t)
	resp := h.do(t, "PUT", "/v1/designer/templates/letter",
		`{"source":"#let d = json(\"data.json\")","defaults":"","example":""}`, h.key)
	if resp.StatusCode != 200 {
		t.Fatalf("save should be 200, got %d", resp.StatusCode)
	}
	if fileExists(filepath.Join(h.templatesDir, "letter.defaults.json")) {
		t.Fatal("empty defaults should remove the defaults file")
	}
	if fileExists(filepath.Join(h.examplesDir, "letter.json")) {
		t.Fatal("empty example should remove the example file")
	}
}

func TestDesignerDelete(t *testing.T) {
	h := newDesignerHarness(t)
	if resp := h.do(t, "DELETE", "/v1/designer/templates/letter", "", h.key); resp.StatusCode != 200 {
		t.Fatalf("delete should be 200, got %d", resp.StatusCode)
	}
	for _, p := range []string{
		filepath.Join(h.templatesDir, "letter.typ"),
		filepath.Join(h.templatesDir, "letter.defaults.json"),
		filepath.Join(h.examplesDir, "letter.json"),
	} {
		if fileExists(p) {
			t.Fatalf("%s should be gone after delete", p)
		}
	}
	if resp := h.do(t, "DELETE", "/v1/designer/templates/letter", "", h.key); resp.StatusCode != 404 {
		t.Fatalf("double delete should be 404, got %d", resp.StatusCode)
	}
}

func TestDesignerPreviewRendersAndReportsDiagnostics(t *testing.T) {
	h := newDesignerHarness(t)

	// Default format is the lightweight per-page SVG preview.
	resp := h.do(t, "POST", "/v1/designer/render",
		`{"source":"= Hello","data":{"x":1},"defaults":"{\"x\":\"—\",\"y\":\"—\"}"}`, h.key)
	if resp.StatusCode != 200 {
		t.Fatalf("svg preview should be 200, got %d", resp.StatusCode)
	}
	svg := decode[map[string][]string](t, resp)
	if len(svg["pages"]) != 2 {
		t.Fatalf("svg preview should return the pages, got %v", svg)
	}

	// PDF only when explicitly requested.
	resp = h.do(t, "POST", "/v1/designer/render", `{"source":"= Hello","format":"pdf"}`, h.key)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/pdf" {
		t.Fatalf("pdf preview should return a PDF, got %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	resp = h.do(t, "POST", "/v1/designer/render", `{"source":"x","format":"gif"}`, h.key)
	if resp.StatusCode != 400 {
		t.Fatalf("unknown format should be 400, got %d", resp.StatusCode)
	}

	resp = h.do(t, "POST", "/v1/designer/render", `{"source":"BROKEN"}`, h.key)
	if resp.StatusCode != 422 {
		t.Fatalf("broken preview should be 422, got %d", resp.StatusCode)
	}
	if body := decode[map[string]string](t, resp); !strings.Contains(body["detail"], "unknown variable") {
		t.Fatalf("preview failure should carry diagnostics: %v", body)
	}
}

func TestDesignerRejectsBadNamesAndRequiresAuth(t *testing.T) {
	h := newDesignerHarness(t)
	if resp := h.do(t, "PUT", "/v1/designer/templates/..%2Fevil", `{"source":"x"}`, h.key); resp.StatusCode != 400 {
		t.Fatalf("traversal name should be 400, got %d", resp.StatusCode)
	}
	if resp := h.do(t, "GET", "/v1/designer/templates", "", ""); resp.StatusCode != 401 {
		t.Fatalf("designer requires auth, got %d", resp.StatusCode)
	}
}
