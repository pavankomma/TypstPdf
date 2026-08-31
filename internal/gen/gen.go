// Package gen renders contract-note PDFs with Typst.
//
// For each job it creates a scratch dir with the client's data.json and
// the shared template, shells out to `typst compile`, and uploads the
// PDF to the object store. The per-job temp files mirror the ~7 million
// intermediate files Zerodha's nightly run produces.
package gen

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/example/contract-notes-pipeline/internal/store"
)

type Generator struct {
	TemplatePath string // templates/contract_note.typ
	WorkDir      string // scratch space for per-job dirs
	Store        *store.Store
}

// Render compiles one client's contract note and returns the object key
// the signed stage should read from.
func (g *Generator) Render(clientID, dataJSON string) (string, error) {
	dir, err := os.MkdirTemp(g.WorkDir, "note-"+clientID+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	if err := os.WriteFile(filepath.Join(dir, "data.json"), []byte(dataJSON), 0o644); err != nil {
		return "", err
	}
	tpl, err := os.ReadFile(g.TemplatePath)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "note.typ"), tpl, 0o644); err != nil {
		return "", err
	}

	pdf := filepath.Join(dir, clientID+".pdf")
	cmd := exec.Command("typst", "compile", "note.typ", pdf)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("typst compile %s: %v\n%s", clientID, err, out)
	}

	key := filepath.Join("pdfs", clientID+".pdf")
	if _, err := g.Store.PutFile(key, pdf); err != nil {
		return "", err
	}
	return key, nil
}
