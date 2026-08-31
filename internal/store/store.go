// Package store is the SQLite system of record: API keys, the render job
// queue, and — because every job row carries who/what/when/outcome plus
// the artifact's sha256 and signature — the audit trail.
//
// SQLite runs in WAL mode with a busy timeout, so concurrent readers are
// cheap and the single-writer constraint only serializes the (tiny)
// queue-state updates, never the renders themselves.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Job statuses. queued → running → succeeded | failed (failed only after
// max_attempts; earlier failures go back to queued with a run_after gate).
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

var (
	// ErrNotFound is returned for a missing job or API key.
	ErrNotFound = errors.New("not found")
	// ErrUnauthorized is returned when an API key does not resolve.
	ErrUnauthorized = errors.New("unauthorized")
)

type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database and applies
// migrations. The DSN enables WAL, a 5s busy timeout, and foreign keys.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrations are applied in order; PRAGMA user_version tracks progress.
var migrations = []string{`
CREATE TABLE api_keys (
  id         INTEGER PRIMARY KEY,
  name       TEXT UNIQUE NOT NULL,
  key_sha256 BLOB UNIQUE NOT NULL,
  templates  TEXT NOT NULL DEFAULT '*',
  disabled   INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
CREATE TABLE jobs (
  id                TEXT PRIMARY KEY,
  api_key_id        INTEGER NOT NULL REFERENCES api_keys(id),
  template          TEXT NOT NULL,
  template_version  TEXT NOT NULL DEFAULT '',
  data              TEXT NOT NULL,
  data_sha256       TEXT NOT NULL,
  filename          TEXT NOT NULL DEFAULT '',
  status            TEXT NOT NULL CHECK(status IN ('queued','running','succeeded','failed')),
  attempts          INTEGER NOT NULL DEFAULT 0,
  max_attempts      INTEGER NOT NULL DEFAULT 3,
  run_after         TEXT,
  error             TEXT NOT NULL DEFAULT '',
  pdf_path          TEXT NOT NULL DEFAULT '',
  pdf_sha256        TEXT NOT NULL DEFAULT '',
  pdf_bytes         INTEGER NOT NULL DEFAULT 0,
  pdf_standard      TEXT NOT NULL DEFAULT '',
  archival_fallback INTEGER NOT NULL DEFAULT 0,
  signature         TEXT NOT NULL DEFAULT '',
  sync              INTEGER NOT NULL DEFAULT 0,
  created_at        TEXT NOT NULL,
  started_at        TEXT,
  finished_at       TEXT
);
CREATE INDEX jobs_claim ON jobs(status, run_after, created_at);
CREATE INDEX jobs_by_key ON jobs(api_key_id, created_at DESC);
`}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ---- time helpers -----------------------------------------------------
// All timestamps are stored as RFC3339Nano UTC strings, which sort
// lexicographically in creation order.

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s.String)
	if err != nil {
		return nil
	}
	return &t
}

// ---- API keys ---------------------------------------------------------

type APIKey struct {
	ID        int64
	Name      string
	Templates string // "*" or a JSON array of allowed template names
	Disabled  bool
	CreatedAt time.Time
}

// Allows reports whether the key may render the named template.
func (k *APIKey) Allows(template string) bool {
	if k.Templates == "*" || k.Templates == "" {
		return true
	}
	var list []string
	if err := json.Unmarshal([]byte(k.Templates), &list); err != nil {
		return false // malformed allowlist fails closed
	}
	return slices.Contains(list, template)
}

func hashKey(plaintext string) []byte {
	h := sha256.Sum256([]byte(plaintext))
	return h[:]
}

// CreateKey mints a new API key and returns its plaintext exactly once;
// only the sha256 is stored. templates nil/empty = all templates.
func (s *Store) CreateKey(name string, templates []string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	plaintext := "tp_" + base64.RawURLEncoding.EncodeToString(raw)
	allow := "*"
	if len(templates) > 0 {
		b, err := json.Marshal(templates)
		if err != nil {
			return "", err
		}
		allow = string(b)
	}
	_, err := s.db.Exec(
		"INSERT INTO api_keys (name, key_sha256, templates, created_at) VALUES (?,?,?,?)",
		name, hashKey(plaintext), allow, ts(time.Now()),
	)
	if err != nil {
		return "", fmt.Errorf("create key %q: %w", name, err)
	}
	return plaintext, nil
}

// LookupKey resolves a plaintext bearer key to an enabled API key.
func (s *Store) LookupKey(plaintext string) (*APIKey, error) {
	row := s.db.QueryRow(
		"SELECT id, name, templates, disabled, created_at, key_sha256 FROM api_keys WHERE key_sha256 = ?",
		hashKey(plaintext),
	)
	var k APIKey
	var disabled int
	var created string
	var storedHash []byte
	if err := row.Scan(&k.ID, &k.Name, &k.Templates, &disabled, &created, &storedHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrUnauthorized
		}
		return nil, err
	}
	// The unique index did the lookup; compare again in constant time as
	// a belt-and-braces measure.
	if subtle.ConstantTimeCompare(storedHash, hashKey(plaintext)) != 1 || disabled != 0 {
		return nil, ErrUnauthorized
	}
	k.Disabled = disabled != 0
	if t := parseTS(sql.NullString{String: created, Valid: true}); t != nil {
		k.CreatedAt = *t
	}
	return &k, nil
}

// EnsureKey returns the named key's ID, creating it (all templates
// allowed) if absent. Used for the -no-auth dev identity so job rows
// always have an owner.
func (s *Store) EnsureKey(name string) (int64, error) {
	var id int64
	err := s.db.QueryRow("SELECT id FROM api_keys WHERE name = ?", name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if _, err := s.CreateKey(name, nil); err != nil {
		return 0, err
	}
	err = s.db.QueryRow("SELECT id FROM api_keys WHERE name = ?", name).Scan(&id)
	return id, err
}

func (s *Store) ListKeys() ([]APIKey, error) {
	rows, err := s.db.Query("SELECT id, name, templates, disabled, created_at FROM api_keys ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []APIKey
	for rows.Next() {
		var k APIKey
		var disabled int
		var created string
		if err := rows.Scan(&k.ID, &k.Name, &k.Templates, &disabled, &created); err != nil {
			return nil, err
		}
		k.Disabled = disabled != 0
		if t := parseTS(sql.NullString{String: created, Valid: true}); t != nil {
			k.CreatedAt = *t
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (s *Store) DisableKey(name string) error {
	res, err := s.db.Exec("UPDATE api_keys SET disabled = 1 WHERE name = ?", name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("key %q: %w", name, ErrNotFound)
	}
	return nil
}

// ---- Jobs -------------------------------------------------------------

type Job struct {
	ID               string
	APIKeyID         int64
	Template         string
	TemplateVersion  string
	Data             []byte
	DataSha256       string
	Filename         string
	Status           string
	Attempts         int
	MaxAttempts      int
	RunAfter         *time.Time
	Error            string
	PDFPath          string
	PDFSha256        string
	PDFBytes         int64
	PDFStandard      string
	ArchivalFallback bool
	Signature        string
	Sync             bool
	CreatedAt        time.Time
	StartedAt        *time.Time
	FinishedAt       *time.Time
}

// Artifact carries the facts of a completed render into the job row.
type Artifact struct {
	TemplateVersion  string
	PDFPath          string
	PDFSha256        string
	PDFBytes         int64
	PDFStandard      string
	ArchivalFallback bool
	Signature        string
}

// NewJobID mints a 32-hex random job ID. Exported so the sync render
// path can name the artifact file after the job before inserting the row.
func NewJobID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

const jobColumns = `id, api_key_id, template, template_version, data, data_sha256,
 filename, status, attempts, max_attempts, run_after, error, pdf_path, pdf_sha256,
 pdf_bytes, pdf_standard, archival_fallback, signature, sync, created_at, started_at, finished_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanJob(r rowScanner) (*Job, error) {
	var j Job
	var data string
	var runAfter, startedAt, finishedAt sql.NullString
	var archival, syncFlag int
	var created string
	err := r.Scan(
		&j.ID, &j.APIKeyID, &j.Template, &j.TemplateVersion, &data, &j.DataSha256,
		&j.Filename, &j.Status, &j.Attempts, &j.MaxAttempts, &runAfter, &j.Error,
		&j.PDFPath, &j.PDFSha256, &j.PDFBytes, &j.PDFStandard, &archival,
		&j.Signature, &syncFlag, &created, &startedAt, &finishedAt,
	)
	if err != nil {
		return nil, err
	}
	j.Data = []byte(data)
	j.ArchivalFallback = archival != 0
	j.Sync = syncFlag != 0
	j.RunAfter = parseTS(runAfter)
	j.StartedAt = parseTS(startedAt)
	j.FinishedAt = parseTS(finishedAt)
	if t := parseTS(sql.NullString{String: created, Valid: true}); t != nil {
		j.CreatedAt = *t
	}
	return &j, nil
}

// Enqueue records a new queued render job and returns it.
func (s *Store) Enqueue(apiKeyID int64, template string, data []byte, filename string, maxAttempts int) (*Job, error) {
	id, err := NewJobID()
	if err != nil {
		return nil, err
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	sum := sha256.Sum256(data)
	now := time.Now()
	_, err = s.db.Exec(`INSERT INTO jobs
		(id, api_key_id, template, data, data_sha256, filename, status, max_attempts, created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		id, apiKeyID, template, string(data), hex.EncodeToString(sum[:]),
		filename, StatusQueued, maxAttempts, ts(now),
	)
	if err != nil {
		return nil, err
	}
	return s.GetJob(id, 0)
}

// RecordSyncJob writes an already-finished job row for the inline /render
// path, so synchronous renders land in the same audit trail. id may be ""
// to mint one; artifact is nil for a failed render; errMsg is "" for a
// successful one.
func (s *Store) RecordSyncJob(id string, apiKeyID int64, template string, data []byte, filename string, artifact *Artifact, errMsg string) (*Job, error) {
	if id == "" {
		var err error
		if id, err = NewJobID(); err != nil {
			return nil, err
		}
	}
	sum := sha256.Sum256(data)
	now := ts(time.Now())
	status := StatusSucceeded
	a := artifact
	if a == nil {
		a = &Artifact{}
		status = StatusFailed
	}
	_, err := s.db.Exec(`INSERT INTO jobs
		(id, api_key_id, template, template_version, data, data_sha256, filename,
		 status, attempts, max_attempts, error, pdf_path, pdf_sha256, pdf_bytes,
		 pdf_standard, archival_fallback, signature, sync, created_at, started_at, finished_at)
		VALUES (?,?,?,?,?,?,?,?,1,1,?,?,?,?,?,?,?,1,?,?,?)`,
		id, apiKeyID, template, a.TemplateVersion, string(data), hex.EncodeToString(sum[:]),
		filename, status, errMsg, a.PDFPath, a.PDFSha256, a.PDFBytes,
		a.PDFStandard, boolInt(a.ArchivalFallback), a.Signature, now, now, now,
	)
	if err != nil {
		return nil, err
	}
	return s.GetJob(id, 0)
}

// Claim atomically takes the oldest runnable queued job, marking it
// running and bumping attempts. Returns (nil, nil) when the queue is
// empty.
func (s *Store) Claim(now time.Time) (*Job, error) {
	row := s.db.QueryRow(`UPDATE jobs
		SET status = ?, started_at = ?, attempts = attempts + 1
		WHERE id = (
			SELECT id FROM jobs
			WHERE status = ? AND (run_after IS NULL OR run_after <= ?)
			ORDER BY created_at LIMIT 1
		)
		RETURNING `+jobColumns,
		StatusRunning, ts(now), StatusQueued, ts(now),
	)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// Complete marks a running job succeeded with its artifact facts.
func (s *Store) Complete(id string, a Artifact) error {
	res, err := s.db.Exec(`UPDATE jobs SET
		status = ?, template_version = ?, pdf_path = ?, pdf_sha256 = ?, pdf_bytes = ?,
		pdf_standard = ?, archival_fallback = ?, signature = ?, error = '', finished_at = ?
		WHERE id = ? AND status = ?`,
		StatusSucceeded, a.TemplateVersion, a.PDFPath, a.PDFSha256, a.PDFBytes,
		a.PDFStandard, boolInt(a.ArchivalFallback), a.Signature, ts(time.Now()),
		id, StatusRunning,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("complete job %s: not running: %w", id, ErrNotFound)
	}
	return nil
}

// Fail records a render failure: back to queued with exponential backoff
// (2^attempts seconds) while attempts remain, else terminally failed.
// Returns the resulting status.
func (s *Store) Fail(id string, errMsg string) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var attempts, maxAttempts int
	err = tx.QueryRow("SELECT attempts, max_attempts FROM jobs WHERE id = ? AND status = ?",
		id, StatusRunning).Scan(&attempts, &maxAttempts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("fail job %s: not running: %w", id, ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	now := time.Now()
	if attempts >= maxAttempts {
		_, err = tx.Exec("UPDATE jobs SET status = ?, error = ?, finished_at = ? WHERE id = ?",
			StatusFailed, errMsg, ts(now), id)
		if err != nil {
			return "", err
		}
		return StatusFailed, tx.Commit()
	}
	backoff := time.Duration(1<<uint(attempts)) * time.Second
	_, err = tx.Exec("UPDATE jobs SET status = ?, error = ?, run_after = ?, started_at = NULL WHERE id = ?",
		StatusQueued, errMsg, ts(now.Add(backoff)), id)
	if err != nil {
		return "", err
	}
	return StatusQueued, tx.Commit()
}

// RecoverRunning requeues jobs stranded in 'running' by a crash. Called
// once at startup (single-process service, so any running row is stale).
func (s *Store) RecoverRunning() (int64, error) {
	res, err := s.db.Exec("UPDATE jobs SET status = ?, started_at = NULL WHERE status = ?",
		StatusQueued, StatusRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetJob fetches a job; apiKeyID > 0 scopes the read to that key's jobs.
func (s *Store) GetJob(id string, apiKeyID int64) (*Job, error) {
	q := "SELECT " + jobColumns + " FROM jobs WHERE id = ?"
	args := []any{id}
	if apiKeyID > 0 {
		q += " AND api_key_id = ?"
		args = append(args, apiKeyID)
	}
	j, err := scanJob(s.db.QueryRow(q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// ListJobs returns the key's jobs, newest first, optionally filtered.
func (s *Store) ListJobs(apiKeyID int64, status, template string, limit int) ([]*Job, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	q := "SELECT " + jobColumns + " FROM jobs WHERE api_key_id = ?"
	args := []any{apiKeyID}
	if status != "" {
		q += " AND status = ?"
		args = append(args, status)
	}
	if template != "" {
		q += " AND template = ?"
		args = append(args, template)
	}
	q += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// QueueDepth counts jobs waiting or in flight (for the metrics gauge).
func (s *Store) QueueDepth() (int64, error) {
	var n int64
	err := s.db.QueryRow("SELECT COUNT(*) FROM jobs WHERE status IN (?, ?)",
		StatusQueued, StatusRunning).Scan(&n)
	return n, err
}

// PurgeBefore deletes finished jobs older than cutoff and returns the
// artifact paths whose files the caller should remove.
func (s *Store) PurgeBefore(cutoff time.Time) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(
		"SELECT pdf_path FROM jobs WHERE status IN (?, ?) AND finished_at < ?",
		StatusSucceeded, StatusFailed, ts(cutoff))
	if err != nil {
		return nil, err
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		if strings.TrimSpace(p) != "" {
			paths = append(paths, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := tx.Exec("DELETE FROM jobs WHERE status IN (?, ?) AND finished_at < ?",
		StatusSucceeded, StatusFailed, ts(cutoff)); err != nil {
		return nil, err
	}
	return paths, tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
