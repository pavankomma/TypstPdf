package worker

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pavankomma/TypstPdf/internal/render"
	"github.com/pavankomma/TypstPdf/internal/sign"
	"github.com/pavankomma/TypstPdf/internal/store"
)

// fakeRenderer scripts render outcomes without needing typst.
type fakeRenderer struct {
	failuresLeft atomic.Int32 // fail this many renders, then succeed
	calls        atomic.Int32
	fallback     bool
}

func (f *fakeRenderer) Render(_ context.Context, template string, _ []byte) (*render.Result, error) {
	f.calls.Add(1)
	if f.failuresLeft.Add(-1) >= 0 {
		return nil, &render.CompileError{Template: template, Output: "scripted failure"}
	}
	pdf := append([]byte("%PDF-1.7\nfake "+template), make([]byte, 600)...)
	return &render.Result{PDF: pdf, Sha256: sha256.Sum256(pdf), ArchivalFallback: f.fallback}, nil
}

func newHarness(t *testing.T, r Renderer) (*store.Store, *Pool, int64) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	signer, err := sign.Load(filepath.Join(dir, "signing.key"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := s.CreateKey("ci", nil)
	if err != nil {
		t.Fatal(err)
	}
	k, err := s.LookupKey(plain)
	if err != nil {
		t.Fatal(err)
	}
	pool := &Pool{
		Store:        s,
		Renderer:     r,
		Signer:       signer,
		ArtifactsDir: filepath.Join(dir, "artifacts"),
		TemplatesDir: filepath.Join("..", "..", "templates"),
		PDFStandard:  "a-2b",
		Workers:      2,
	}
	return s, pool, k.ID
}

// waitForStatus polls until the job reaches want or the deadline passes.
func waitForStatus(t *testing.T, s *store.Store, id, want string) *store.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		j, err := s.GetJob(id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status == want {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _ := s.GetJob(id, 0)
	t.Fatalf("job never reached %q; last state: %+v", want, j)
	return nil
}

func TestPoolRendersQueuedJob(t *testing.T) {
	fake := &fakeRenderer{}
	s, pool, keyID := newHarness(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	j, err := s.Enqueue(keyID, "invoice", []byte(`{"total":"5"}`), "inv.pdf", 3)
	if err != nil {
		t.Fatal(err)
	}
	pool.Nudge()

	got := waitForStatus(t, s, j.ID, store.StatusSucceeded)
	if got.PDFPath == "" || got.PDFSha256 == "" || got.Signature == "" {
		t.Fatalf("completed job missing artifact facts: %+v", got)
	}
	if got.TemplateVersion == "" {
		t.Fatal("completed job should carry a template version stamp")
	}
	if got.PDFStandard != "a-2b" {
		t.Fatalf("pdf standard should be recorded, got %q", got.PDFStandard)
	}

	// The artifact file exists and the signature verifies against it.
	pdf, err := os.ReadFile(got.PDFPath)
	if err != nil {
		t.Fatalf("artifact file should exist: %v", err)
	}
	ok, err := sign.Verify(pool.Signer.PublicKey(), pdf, got.Signature)
	if err != nil || !ok {
		t.Fatalf("stored signature must verify against the artifact: %v %v", ok, err)
	}

	cancel()
	<-done
}

func TestPoolRetriesThenSucceeds(t *testing.T) {
	fake := &fakeRenderer{}
	fake.failuresLeft.Store(1) // first attempt fails, second succeeds
	s, pool, keyID := newHarness(t, fake)

	j, err := s.Enqueue(keyID, "invoice", []byte(`{}`), "", 3)
	if err != nil {
		t.Fatal(err)
	}

	// Drive the queue manually so the test doesn't wait out the real
	// backoff: claim/fail/claim mirrors what the pool's loop does.
	claimed, _ := s.Claim(time.Now())
	pool.process(context.Background(), claimed)
	mid, _ := s.GetJob(j.ID, 0)
	if mid.Status != store.StatusQueued || mid.Error == "" {
		t.Fatalf("first failure should requeue with the error recorded: %+v", mid)
	}

	claimed, _ = s.Claim(time.Now().Add(time.Hour)) // past the backoff gate
	pool.process(context.Background(), claimed)
	got, _ := s.GetJob(j.ID, 0)
	if got.Status != store.StatusSucceeded || got.Error != "" {
		t.Fatalf("second attempt should succeed and clear the error: %+v", got)
	}
	if fake.calls.Load() != 2 {
		t.Fatalf("renderer should have been called twice, got %d", fake.calls.Load())
	}
}

func TestPoolExhaustsAttemptsToFailure(t *testing.T) {
	fake := &fakeRenderer{}
	fake.failuresLeft.Store(10) // always fails
	s, pool, keyID := newHarness(t, fake)

	j, err := s.Enqueue(keyID, "invoice", []byte(`{}`), "", 2)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		claimed, _ := s.Claim(time.Now().Add(time.Hour))
		if claimed == nil {
			t.Fatalf("attempt %d: expected a claimable job", i+1)
		}
		pool.process(context.Background(), claimed)
	}
	got, _ := s.GetJob(j.ID, 0)
	if got.Status != store.StatusFailed {
		t.Fatalf("job should be terminally failed: %+v", got)
	}
	if got.Error == "" || !strings.Contains(got.Error, "scripted failure") {
		t.Fatalf("failed job should carry typst diagnostics: %q", got.Error)
	}
}

func TestArchivalFallbackIsRecorded(t *testing.T) {
	fake := &fakeRenderer{fallback: true}
	s, pool, keyID := newHarness(t, fake)
	j, _ := s.Enqueue(keyID, "invoice", []byte(`{}`), "", 3)
	claimed, _ := s.Claim(time.Now())
	pool.process(context.Background(), claimed)
	got, _ := s.GetJob(j.ID, 0)
	if !got.ArchivalFallback || got.PDFStandard != "baseline-fallback" {
		t.Fatalf("fallback must be recorded on the row: %+v", got)
	}
}

func TestTemplateVersionIsStableAndContentSensitive(t *testing.T) {
	dir := t.TempDir()
	typ := filepath.Join(dir, "x.typ")
	os.WriteFile(typ, []byte("v1"), 0o644)

	v1 := TemplateVersion(dir, "x")
	if v1 == "" || len(v1) != 12 {
		t.Fatalf("version should be 12 hex chars, got %q", v1)
	}
	if again := TemplateVersion(dir, "x"); again != v1 {
		t.Fatal("version must be stable for unchanged content")
	}

	// Editing the template changes the version…
	os.WriteFile(typ, []byte("v2"), 0o644)
	v2 := TemplateVersion(dir, "x")
	if v2 == v1 {
		t.Fatal("template edit must change the version")
	}
	// …and so does editing the defaults file.
	os.WriteFile(filepath.Join(dir, "x.defaults.json"), []byte(`{}`), 0o644)
	if v3 := TemplateVersion(dir, "x"); v3 == v2 {
		t.Fatal("defaults edit must change the version")
	}

	if TemplateVersion(dir, "missing") != "" {
		t.Fatal("missing template should yield an empty version")
	}
}
