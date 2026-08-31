package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newTestKey(t *testing.T, s *Store, name string, templates []string) (int64, string) {
	t.Helper()
	plain, err := s.CreateKey(name, templates)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	k, err := s.LookupKey(plain)
	if err != nil {
		t.Fatalf("lookup created key: %v", err)
	}
	return k.ID, plain
}

// ---- API keys ---------------------------------------------------------

func TestKeyLifecycle(t *testing.T) {
	s := newTestStore(t)
	id, plain := newTestKey(t, s, "ci", nil)
	if !strings.HasPrefix(plain, "tp_") {
		t.Fatalf("key should carry the tp_ prefix: %q", plain)
	}
	if id == 0 {
		t.Fatal("key id should be assigned")
	}

	// Wrong key → unauthorized.
	if _, err := s.LookupKey("tp_bogus"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bogus key should be ErrUnauthorized, got %v", err)
	}

	// Disabled key → unauthorized.
	if err := s.DisableKey("ci"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupKey(plain); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("disabled key should be ErrUnauthorized, got %v", err)
	}

	// Disabling an unknown key is a NotFound.
	if err := s.DisableKey("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown key disable should be ErrNotFound, got %v", err)
	}
}

func TestKeyTemplateAllowlist(t *testing.T) {
	s := newTestStore(t)
	_, plain := newTestKey(t, s, "letters-only", []string{"letter", "report"})
	k, err := s.LookupKey(plain)
	if err != nil {
		t.Fatal(err)
	}
	if !k.Allows("letter") || !k.Allows("report") {
		t.Fatal("allowlisted templates should be allowed")
	}
	if k.Allows("invoice") {
		t.Fatal("non-listed template should be denied")
	}

	_, plainAll := newTestKey(t, s, "all", nil)
	kAll, err := s.LookupKey(plainAll)
	if err != nil {
		t.Fatal(err)
	}
	if !kAll.Allows("anything") {
		t.Fatal("wildcard key should allow everything")
	}
}

func TestEnsureKeyIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	id1, err := s.EnsureKey("anonymous")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.EnsureKey("anonymous")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("EnsureKey should be idempotent: %d != %d", id1, id2)
	}
}

// ---- Job queue --------------------------------------------------------

func TestJobLifecycleSuccess(t *testing.T) {
	s := newTestStore(t)
	keyID, _ := newTestKey(t, s, "ci", nil)

	j, err := s.Enqueue(keyID, "invoice", []byte(`{"total":"1"}`), "inv.pdf", 3)
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != StatusQueued || j.DataSha256 == "" || j.ID == "" {
		t.Fatalf("unexpected enqueued job: %+v", j)
	}

	claimed, err := s.Claim(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != j.ID || claimed.Status != StatusRunning || claimed.Attempts != 1 {
		t.Fatalf("unexpected claim: %+v", claimed)
	}

	// Queue is now empty.
	if again, err := s.Claim(time.Now()); err != nil || again != nil {
		t.Fatalf("second claim should be empty, got %+v, %v", again, err)
	}

	if err := s.Complete(j.ID, Artifact{
		TemplateVersion: "abc123def456", PDFPath: "data/artifacts/x.pdf",
		PDFSha256: "ff", PDFBytes: 1234, PDFStandard: "a-2b", Signature: "sig",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(j.ID, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSucceeded || got.PDFBytes != 1234 ||
		got.TemplateVersion != "abc123def456" || got.FinishedAt == nil {
		t.Fatalf("unexpected completed job: %+v", got)
	}
}

func TestJobRetryBackoffAndExhaustion(t *testing.T) {
	s := newTestStore(t)
	keyID, _ := newTestKey(t, s, "ci", nil)
	j, err := s.Enqueue(keyID, "invoice", []byte(`{}`), "", 2)
	if err != nil {
		t.Fatal(err)
	}

	// Attempt 1 fails → requeued with a future run_after.
	if _, err := s.Claim(time.Now()); err != nil {
		t.Fatal(err)
	}
	status, err := s.Fail(j.ID, "boom 1")
	if err != nil || status != StatusQueued {
		t.Fatalf("first failure should requeue, got %q, %v", status, err)
	}
	got, _ := s.GetJob(j.ID, 0)
	if got.RunAfter == nil || !got.RunAfter.After(time.Now()) {
		t.Fatalf("requeued job should carry a future run_after: %+v", got.RunAfter)
	}

	// Not claimable before run_after…
	if c, err := s.Claim(time.Now()); err != nil || c != nil {
		t.Fatalf("backoff-gated job must not be claimable now, got %+v, %v", c, err)
	}
	// …but claimable at a time past the gate.
	c, err := s.Claim(time.Now().Add(time.Hour))
	if err != nil || c == nil || c.Attempts != 2 {
		t.Fatalf("job should be claimable past run_after, got %+v, %v", c, err)
	}

	// Attempt 2 (== max_attempts) fails → terminal.
	status, err = s.Fail(j.ID, "boom 2")
	if err != nil || status != StatusFailed {
		t.Fatalf("final failure should be terminal, got %q, %v", status, err)
	}
	got, _ = s.GetJob(j.ID, 0)
	if got.Status != StatusFailed || got.Error != "boom 2" || got.FinishedAt == nil {
		t.Fatalf("unexpected failed job: %+v", got)
	}
}

func TestClaimOrdersOldestFirst(t *testing.T) {
	s := newTestStore(t)
	keyID, _ := newTestKey(t, s, "ci", nil)
	j1, _ := s.Enqueue(keyID, "a", []byte(`{}`), "", 3)
	time.Sleep(2 * time.Millisecond) // distinct created_at
	s.Enqueue(keyID, "b", []byte(`{}`), "", 3)
	c, err := s.Claim(time.Now())
	if err != nil || c == nil || c.ID != j1.ID {
		t.Fatalf("oldest job should be claimed first, got %+v, %v", c, err)
	}
}

func TestRecoverRunning(t *testing.T) {
	s := newTestStore(t)
	keyID, _ := newTestKey(t, s, "ci", nil)
	j, _ := s.Enqueue(keyID, "invoice", []byte(`{}`), "", 3)
	if _, err := s.Claim(time.Now()); err != nil {
		t.Fatal(err)
	}
	n, err := s.RecoverRunning()
	if err != nil || n != 1 {
		t.Fatalf("expected 1 recovered job, got %d, %v", n, err)
	}
	got, _ := s.GetJob(j.ID, 0)
	if got.Status != StatusQueued {
		t.Fatalf("recovered job should be queued: %+v", got)
	}
}

func TestGetJobScopedToKey(t *testing.T) {
	s := newTestStore(t)
	keyA, _ := newTestKey(t, s, "a", nil)
	keyB, _ := newTestKey(t, s, "b", nil)
	j, _ := s.Enqueue(keyA, "invoice", []byte(`{}`), "", 3)

	if _, err := s.GetJob(j.ID, keyA); err != nil {
		t.Fatalf("owner should read its job: %v", err)
	}
	if _, err := s.GetJob(j.ID, keyB); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another key must not see the job, got %v", err)
	}
}

func TestListJobsFilters(t *testing.T) {
	s := newTestStore(t)
	keyID, _ := newTestKey(t, s, "ci", nil)
	s.Enqueue(keyID, "invoice", []byte(`{}`), "", 3)
	j2, _ := s.Enqueue(keyID, "letter", []byte(`{}`), "", 3)

	all, err := s.ListJobs(keyID, "", "", 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("expected 2 jobs, got %d, %v", len(all), err)
	}
	letters, err := s.ListJobs(keyID, "", "letter", 0)
	if err != nil || len(letters) != 1 || letters[0].ID != j2.ID {
		t.Fatalf("template filter failed: %+v, %v", letters, err)
	}
	queued, err := s.ListJobs(keyID, StatusQueued, "", 0)
	if err != nil || len(queued) != 2 {
		t.Fatalf("status filter failed: %d, %v", len(queued), err)
	}
}

func TestRecordSyncJob(t *testing.T) {
	s := newTestStore(t)
	keyID, _ := newTestKey(t, s, "ci", nil)

	ok, err := s.RecordSyncJob(keyID, "invoice", []byte(`{}`), "x.pdf",
		&Artifact{TemplateVersion: "v1", PDFPath: "p", PDFSha256: "ff", PDFBytes: 9}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !ok.Sync || ok.Status != StatusSucceeded || ok.FinishedAt == nil {
		t.Fatalf("unexpected sync job: %+v", ok)
	}

	bad, err := s.RecordSyncJob(keyID, "invoice", []byte(`{}`), "", nil, "compile exploded")
	if err != nil {
		t.Fatal(err)
	}
	if bad.Status != StatusFailed || bad.Error != "compile exploded" {
		t.Fatalf("unexpected failed sync job: %+v", bad)
	}
	// Sync rows must never be claimable by the worker pool.
	if c, err := s.Claim(time.Now()); err != nil || c != nil {
		t.Fatalf("sync rows must not enter the queue, got %+v, %v", c, err)
	}
}

func TestQueueDepthAndPurge(t *testing.T) {
	s := newTestStore(t)
	keyID, _ := newTestKey(t, s, "ci", nil)
	j, _ := s.Enqueue(keyID, "invoice", []byte(`{}`), "", 3)
	if n, _ := s.QueueDepth(); n != 1 {
		t.Fatalf("queue depth should be 1, got %d", n)
	}
	s.Claim(time.Now())
	s.Complete(j.ID, Artifact{PDFPath: "data/artifacts/gone.pdf"})
	if n, _ := s.QueueDepth(); n != 0 {
		t.Fatalf("queue depth should be 0 after completion, got %d", n)
	}

	// Purge with a future cutoff removes the finished job and reports its
	// artifact path; queued/running jobs are never purged.
	s.Enqueue(keyID, "letter", []byte(`{}`), "", 3)
	paths, err := s.PurgeBefore(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "data/artifacts/gone.pdf" {
		t.Fatalf("expected the finished artifact path, got %v", paths)
	}
	if _, err := s.GetJob(j.ID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("purged job should be gone, got %v", err)
	}
	if remaining, _ := s.ListJobs(keyID, "", "", 0); len(remaining) != 1 {
		t.Fatalf("queued job should survive the purge, got %d rows", len(remaining))
	}
}

func TestMigrationsAreIdempotentAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	keyID, _ := newTestKey(t, s1, "ci", nil)
	j, _ := s1.Enqueue(keyID, "invoice", []byte(`{}`), "", 3)
	s1.Close()

	s2, err := Open(path) // reopen: migrations must not re-run
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got, err := s2.GetJob(j.ID, 0)
	if err != nil || got.Template != "invoice" {
		t.Fatalf("data should survive reopen: %+v, %v", got, err)
	}
}
