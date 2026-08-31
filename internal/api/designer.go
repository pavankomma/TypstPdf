// Designer/template-manager endpoints: browse, edit, validate, save, and
// preview templates through the real render pipeline. Registered only
// when the server runs with -designer — the raw-source preview endpoint
// can compile arbitrary Typst, so this surface is for trusted dev/admin
// environments, never the open internet.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/pavankomma/TypstPdf/internal/render"
	"github.com/pavankomma/TypstPdf/internal/worker"
)

// SourceRenderer is the designer's slice of *render.Renderer: compile an
// in-memory template source to PDF, or to per-page SVGs for the inline
// preview. Faked in tests.
type SourceRenderer interface {
	RenderSource(ctx context.Context, source, data []byte) (*render.Result, error)
	RenderSourceSVG(ctx context.Context, source, data []byte) ([]string, error)
}

// registerDesigner mounts the designer API (called from Handler when
// s.Designer is set).
func (s *Server) registerDesigner(mux *http.ServeMux) {
	mux.Handle("GET /v1/designer/templates", s.authed(s.designerList))
	mux.Handle("GET /v1/designer/templates/{name}", s.authed(s.designerGet))
	mux.Handle("PUT /v1/designer/templates/{name}", s.authed(s.designerPut))
	mux.Handle("DELETE /v1/designer/templates/{name}", s.authed(s.designerDelete))
	mux.Handle("POST /v1/designer/render", s.authed(s.designerRender))
	if s.DesignerUI != nil {
		mux.Handle("GET /designer/", http.StripPrefix("/designer/", http.FileServerFS(s.DesignerUI)))
		mux.Handle("GET /designer", http.RedirectHandler("/designer/", http.StatusMovedPermanently))
	}
}

type designerTemplate struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	HasDefaults bool   `json:"has_defaults"`
	HasExample  bool   `json:"has_example"`
}

func (s *Server) designerList(w http.ResponseWriter, r *http.Request) {
	names, err := s.ListTemplates()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]designerTemplate, 0, len(names))
	for _, n := range names {
		out = append(out, designerTemplate{
			Name:        n,
			Version:     worker.TemplateVersion(s.TemplatesDir, n),
			HasDefaults: fileExists(filepath.Join(s.TemplatesDir, n+".defaults.json")),
			HasExample:  fileExists(filepath.Join(s.ExamplesDir, n+".json")),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

// designerDoc is a template's full editable state on the wire.
type designerDoc struct {
	Name     string `json:"name"`
	Version  string `json:"version,omitempty"`
	Source   string `json:"source"`
	Defaults string `json:"defaults"` // raw JSON text ("" = no defaults file)
	Example  string `json:"example"`  // raw JSON text ("" = no example file)
}

func (s *Server) designerGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := render.ValidateTemplateName(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	source, err := os.ReadFile(filepath.Join(s.TemplatesDir, name+".typ"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown template " + name})
		return
	}
	doc := designerDoc{
		Name:    name,
		Version: worker.TemplateVersion(s.TemplatesDir, name),
		Source:  string(source),
	}
	if b, err := os.ReadFile(filepath.Join(s.TemplatesDir, name+".defaults.json")); err == nil {
		doc.Defaults = string(b)
	}
	if b, err := os.ReadFile(filepath.Join(s.ExamplesDir, name+".json")); err == nil {
		doc.Example = string(b)
	}
	writeJSON(w, http.StatusOK, doc)
}

// designerPut validates and saves a template: the source must compile
// against its defaults-only payload (the same guarantee the test suite
// enforces) before anything touches disk. Defaults/example semantics:
// non-empty = write, empty string = remove the file.
func (s *Server) designerPut(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := render.ValidateTemplateName(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	body, err := readBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var doc designerDoc
	if err := json.Unmarshal(body, &doc); err != nil || doc.Source == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {source, defaults?, example?}"})
		return
	}
	for label, raw := range map[string]string{"defaults": doc.Defaults, "example": doc.Example} {
		if raw != "" && !json.Valid([]byte(raw)) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": label + " must be valid JSON"})
			return
		}
	}

	// The save gate: render the source with the defaults-only payload so a
	// template that reads a key its defaults don't supply is rejected here,
	// with diagnostics, instead of failing renders later.
	placeholder := []byte(`{}`)
	if doc.Defaults != "" {
		if placeholder, err = render.MergeDefaults([]byte(doc.Defaults), []byte(`{}`)); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	if _, err := s.SourceRenderer.RenderSource(r.Context(), []byte(doc.Source), placeholder); err != nil {
		var ce *render.CompileError
		if errors.As(err, &ce) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "template does not compile against its defaults", "detail": ce.Output,
			})
			return
		}
		slog.Error("designer save validation", "template", name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if err := writeAtomic(filepath.Join(s.TemplatesDir, name+".typ"), []byte(doc.Source)); err != nil {
		slog.Error("designer save", "template", name, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := writeOrRemove(filepath.Join(s.TemplatesDir, name+".defaults.json"), doc.Defaults); err != nil {
		slog.Error("designer save defaults", "template", name, "error", err)
	}
	if s.ExamplesDir != "" {
		if err := writeOrRemove(filepath.Join(s.ExamplesDir, name+".json"), doc.Example); err != nil {
			slog.Error("designer save example", "template", name, "error", err)
		}
	}
	version := worker.TemplateVersion(s.TemplatesDir, name)
	slog.Info("template saved via designer", "template", name, "version", version, "key", callerKey(r).Name)
	writeJSON(w, http.StatusOK, map[string]string{"name": name, "version": version})
}

func (s *Server) designerDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := render.ValidateTemplateName(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	typPath := filepath.Join(s.TemplatesDir, name+".typ")
	if !fileExists(typPath) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown template " + name})
		return
	}
	if err := os.Remove(typPath); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	os.Remove(filepath.Join(s.TemplatesDir, name+".defaults.json"))
	if s.ExamplesDir != "" {
		os.Remove(filepath.Join(s.ExamplesDir, name+".json"))
	}
	slog.Info("template deleted via designer", "template", name, "key", callerKey(r).Name)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": name})
}

// designerRender previews an in-memory source + data (+ optional unsaved
// defaults) through the real pipeline. Ephemeral: no job row, no artifact.
func (s *Server) designerRender(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var req struct {
		Source   string          `json:"source"`
		Data     json.RawMessage `json:"data"`
		Defaults string          `json:"defaults"`
		// "svg" (default) = per-page inline preview JSON; "pdf" = bytes.
		Format string `json:"format"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Source == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {source, data?, defaults?, format?}"})
		return
	}
	if req.Format == "" {
		req.Format = "svg"
	}
	if req.Format != "svg" && req.Format != "pdf" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "format must be svg or pdf"})
		return
	}
	data := []byte(req.Data)
	if len(data) == 0 {
		data = []byte(`{}`)
	}
	if req.Defaults != "" {
		if data, err = render.MergeDefaults([]byte(req.Defaults), data); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	start := time.Now()
	writeRenderErr := func(err error) {
		var ce *render.CompileError
		switch {
		case errors.As(err, &ce):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
				"error": "typst compile failed", "detail": ce.Output,
			})
		case errors.Is(err, context.DeadlineExceeded):
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "compile timed out"})
		default:
			slog.Error("designer preview", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		}
	}

	if req.Format == "svg" {
		pages, err := s.SourceRenderer.RenderSourceSVG(r.Context(), []byte(req.Source), data)
		if err != nil {
			writeRenderErr(err)
			return
		}
		slog.Info("designer preview", "format", "svg", "pages", len(pages),
			"duration", time.Since(start).Round(time.Millisecond).String())
		writeJSON(w, http.StatusOK, map[string]any{"pages": pages})
		return
	}

	res, err := s.SourceRenderer.RenderSource(r.Context(), []byte(req.Source), data)
	if err != nil {
		writeRenderErr(err)
		return
	}
	slog.Info("designer preview", "format", "pdf", "bytes", len(res.PDF),
		"duration", time.Since(start).Round(time.Millisecond).String())
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="preview.pdf"`)
	w.Write(res.PDF)
}

// ---- helpers ----------------------------------------------------------

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writeAtomic(path string, content []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// writeOrRemove writes content, or removes the file when content is "".
func writeOrRemove(path, content string) error {
	if content == "" {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return writeAtomic(path, []byte(content))
}
