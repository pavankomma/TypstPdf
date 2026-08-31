// Package render compiles Typst templates into PDFs.
//
// For each request it creates a scratch dir containing the caller's
// data.json and a copy of the chosen template, shells out to
// `typst compile`, and returns the PDF bytes. A semaphore bounds the
// number of concurrent compiles since typst is CPU-heavy.
package render

import (
	"context"
	"errors"
	"fmt"
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

type Renderer struct {
	TypstBin     string        // path to the typst executable
	TemplatesDir string        // directory holding *.typ templates
	Timeout      time.Duration // per-compile timeout
	sem          chan struct{} // bounds concurrent typst processes
}

func New(typstBin, templatesDir string, concurrency int, timeout time.Duration) *Renderer {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Renderer{
		TypstBin:     typstBin,
		TemplatesDir: templatesDir,
		Timeout:      timeout,
		sem:          make(chan struct{}, concurrency),
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

// Render compiles the named template against the given JSON document and
// returns the PDF. Templates read their input with `json("data.json")`.
func (r *Renderer) Render(ctx context.Context, template string, data []byte) ([]byte, error) {
	if !nameRe.MatchString(template) || strings.Contains(template, "..") {
		return nil, fmt.Errorf("invalid template name %q", template)
	}
	tpl, err := os.ReadFile(filepath.Join(r.TemplatesDir, template+".typ"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("unknown template %q: %w", template, os.ErrNotExist)
		}
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
	cmd := exec.CommandContext(ctx, r.TypstBin, "compile", "main.typ", "out.pdf")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("typst compile %s: %w", template, ctx.Err())
		}
		return nil, &CompileError{Template: template, Output: string(out)}
	}
	return os.ReadFile(filepath.Join(dir, "out.pdf"))
}
