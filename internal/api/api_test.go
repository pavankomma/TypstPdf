package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pavankomma/TypstPdf/internal/render"
	"github.com/pavankomma/TypstPdf/internal/sign"
	"github.com/pavankomma/TypstPdf/internal/store"
	"github.com/pavankomma/TypstPdf/internal/worker"
)

// fakeRenderer succeeds unless the payload contains "explode".
type fakeRenderer struct{}

func (fakeRenderer) Render(_ context.Context, template string, data []byte) (*render.Result, error) {
	if strings.Contains(string(data), "explode") {
		return nil, &render.CompileError{Template: template, Output: "scripted diagnostics"}
	}
	pdf := append([]byte("%PDF-1.7\nfake "+template), make([]byte, 600)...)
	return &render.Result{PDF: pdf, Sha256: sha256.Sum256(pdf)}, nil
}

type harness struct {
	ts    *httptest.Server
	store *store.Store
	pool  *worker.Pool
	key   string // plaintext key, all templates
}

func newHarness(t *testing.T) *harness {
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
	pool := &worker.Pool{
		Store: s, Renderer: fakeRenderer{}, Signer: signer,
		ArtifactsDir: filepath.Join(dir, "artifacts"),
		TemplatesDir: filepath.Join("..", "..", "templates"),
		Workers:      1,
	}
	srv := &Server{
		Store: s, Renderer: fakeRenderer{}, Pool: pool, Signer: signer,
		TemplatesDir:  pool.TemplatesDir,
		ListTemplates: func() ([]string, error) { return []string{"invoice", "letter"}, nil },
		TypstVersion:  "typst test", MaxAttempts: 3,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &harness{ts: ts, store: s, pool: pool, key: plain}
}

func (h *harness) do(t *testing.T, method, path, body, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, h.ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return v
}

// ---- auth -------------------------------------------------------------

func TestAuthRequired(t *testing.T) {
	h := newHarness(t)
	if resp := h.do(t, "GET", "/templates", "", ""); resp.StatusCode != 401 {
		t.Fatalf("no token should be 401, got %d", resp.StatusCode)
	}
	if resp := h.do(t, "GET", "/templates", "", "tp_wrong"); resp.StatusCode != 401 {
		t.Fatalf("wrong token should be 401, got %d", resp.StatusCode)
	}
	if resp := h.do(t, "GET", "/templates", "", h.key); resp.StatusCode != 200 {
		t.Fatalf("valid token should be 200, got %d", resp.StatusCode)
	}
	// Health and metrics stay open.
	if resp := h.do(t, "GET", "/healthz", "", ""); resp.StatusCode != 200 {
		t.Fatalf("healthz should be open, got %d", resp.StatusCode)
	}
	if resp := h.do(t, "GET", "/metrics", "", ""); resp.StatusCode != 200 {
		t.Fatalf("metrics should be open, got %d", resp.StatusCode)
	}
}

func TestTemplateAllowlistEnforced(t *testing.T) {
	h := newHarness(t)
	plain, err := h.store.CreateKey("letters-only", []string{"letter"})
	if err != nil {
		t.Fatal(err)
	}
	if resp := h.do(t, "POST", "/v1/jobs", `{"template":"invoice"}`, plain); resp.StatusCode != 403 {
		t.Fatalf("disallowed template should be 403, got %d", resp.StatusCode)
	}
	if resp := h.do(t, "POST", "/render/invoice", `{}`, plain); resp.StatusCode != 403 {
		t.Fatalf("sync render of disallowed template should be 403, got %d", resp.StatusCode)
	}
	if resp := h.do(t, "POST", "/v1/jobs", `{"template":"letter"}`, plain); resp.StatusCode != 202 {
		t.Fatalf("allowed template should queue, got %d", resp.StatusCode)
	}
}

// ---- async job lifecycle ----------------------------------------------

func TestJobLifecycleOverHTTP(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.pool.Run(ctx)

	resp := h.do(t, "POST", "/v1/jobs", `{"template":"invoice","data":{"total":"9"},"filename":"west.pdf"}`, h.key)
	if resp.StatusCode != 202 {
		t.Fatalf("submit should be 202, got %d", resp.StatusCode)
	}
	job := decode[jobJSON](t, resp)
	if job.ID == "" || job.Status != store.StatusQueued {
		t.Fatalf("unexpected submit response: %+v", job)
	}

	// Poll to completion.
	deadline := time.Now().Add(5 * time.Second)
	var got jobJSON
	for time.Now().Before(deadline) {
		got = decode[jobJSON](t, h.do(t, "GET", "/v1/jobs/"+job.ID, "", h.key))
		if got.Status == store.StatusSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.Status != store.StatusSucceeded {
		t.Fatalf("job never succeeded: %+v", got)
	}
	if got.Signature == "" || got.PDFSha256 == "" || got.TemplateVersion == "" {
		t.Fatalf("completed job missing provenance: %+v", got)
	}

	// Download the artifact.
	dl := h.do(t, "GET", "/v1/jobs/"+job.ID+"/pdf", "", h.key)
	if dl.StatusCode != 200 || dl.Header.Get("Content-Type") != "application/pdf" {
		t.Fatalf("download failed: %d %q", dl.StatusCode, dl.Header.Get("Content-Type"))
	}
	if dl.Header.Get("X-Pdf-Signature") == "" || dl.Header.Get("X-Pdf-Sha256") == "" {
		t.Fatal("download should carry integrity headers")
	}
	if cd := dl.Header.Get("Content-Disposition"); !strings.Contains(cd, "west.pdf") {
		t.Fatalf("requested filename should be honored: %q", cd)
	}
}

func TestGetPDFBeforeCompletionConflicts(t *testing.T) {
	h := newHarness(t) // no pool running: job stays queued
	resp := h.do(t, "POST", "/v1/jobs", `{"template":"invoice"}`, h.key)
	job := decode[jobJSON](t, resp)
	if got := h.do(t, "GET", "/v1/jobs/"+job.ID+"/pdf", "", h.key); got.StatusCode != 409 {
		t.Fatalf("pdf of a queued job should be 409, got %d", got.StatusCode)
	}
}

func TestJobsAreScopedToTheirKey(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, "POST", "/v1/jobs", `{"template":"invoice"}`, h.key)
	job := decode[jobJSON](t, resp)

	other, err := h.store.CreateKey("other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.do(t, "GET", "/v1/jobs/"+job.ID, "", other); got.StatusCode != 404 {
		t.Fatalf("another key must not see the job, got %d", got.StatusCode)
	}
	list := decode[map[string][]jobJSON](t, h.do(t, "GET", "/v1/jobs", "", other))
	if len(list["jobs"]) != 0 {
		t.Fatalf("another key's listing should be empty: %+v", list)
	}
}

func TestUnknownTemplateAndBadBody(t *testing.T) {
	h := newHarness(t)
	if resp := h.do(t, "POST", "/v1/jobs", `{"template":"ghost"}`, h.key); resp.StatusCode != 404 {
		t.Fatalf("unknown template should be 404, got %d", resp.StatusCode)
	}
	if resp := h.do(t, "POST", "/v1/jobs", `not json`, h.key); resp.StatusCode != 400 {
		t.Fatalf("invalid JSON should be 400, got %d", resp.StatusCode)
	}
	if resp := h.do(t, "POST", "/v1/jobs", `{"template":"invoice","filename":"../x.pdf"}`, h.key); resp.StatusCode != 400 {
		t.Fatalf("unsafe filename should be 400, got %d", resp.StatusCode)
	}
}

// ---- sync render ------------------------------------------------------

func TestSyncRenderReturnsPDFAndAuditRow(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, "POST", "/render/invoice?filename=note.pdf", `{"total":"5"}`, h.key)
	if resp.StatusCode != 200 {
		t.Fatalf("sync render should be 200, got %d", resp.StatusCode)
	}
	jobID := resp.Header.Get("X-Job-Id")
	if jobID == "" || resp.Header.Get("X-Pdf-Signature") == "" {
		t.Fatal("sync render should carry job id + signature headers")
	}

	// The render is in the audit trail as a finished sync job.
	j, err := h.store.GetJob(jobID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !j.Sync || j.Status != store.StatusSucceeded || j.Filename != "note.pdf" {
		t.Fatalf("unexpected sync audit row: %+v", j)
	}
}

func TestSyncRenderFailureIsAuditedAnd422(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, "POST", "/render/invoice", `{"explode":true}`, h.key)
	if resp.StatusCode != 422 {
		t.Fatalf("compile failure should be 422, got %d", resp.StatusCode)
	}
	body := decode[map[string]string](t, resp)
	if !strings.Contains(body["detail"], "scripted diagnostics") {
		t.Fatalf("caller should get typst diagnostics: %v", body)
	}
	jobs, err := h.store.ListJobs(1, store.StatusFailed, "", 0)
	if err != nil || len(jobs) != 1 || !jobs[0].Sync {
		t.Fatalf("failed sync render should be audited: %+v, %v", jobs, err)
	}
}

// ---- misc -------------------------------------------------------------

func TestTemplatesEndpointCarriesVersions(t *testing.T) {
	h := newHarness(t)
	resp := h.do(t, "GET", "/templates", "", h.key)
	var out struct {
		Templates []struct{ Name, Version string } `json:"templates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Templates) != 2 {
		t.Fatalf("expected 2 templates, got %+v", out.Templates)
	}
	for _, tpl := range out.Templates {
		// The harness points at the real templates dir, so versions resolve.
		if len(tpl.Version) != 12 {
			t.Fatalf("template %s should have a 12-char version, got %q", tpl.Name, tpl.Version)
		}
	}
}

func TestSigningKeyEndpointVerifiesDownloads(t *testing.T) {
	h := newHarness(t)
	keyResp := decode[map[string]string](t, h.do(t, "GET", "/v1/signing-key", "", h.key))
	if keyResp["algorithm"] != "ed25519" || keyResp["public_key"] == "" {
		t.Fatalf("unexpected signing-key response: %v", keyResp)
	}

	resp := h.do(t, "POST", "/render/invoice", `{}`, h.key)
	sig := resp.Header.Get("X-Pdf-Signature")
	pdf := make([]byte, 0, 1024)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		pdf = append(pdf, buf[:n]...)
		if err != nil {
			break
		}
	}
	ok, err := sign.Verify(keyResp["public_key"], pdf, sig)
	if err != nil || !ok {
		t.Fatalf("download must verify against the published key: %v %v", ok, err)
	}
}

func TestNoAuthModeUsesAnonymousIdentity(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	anonID, err := s.EnsureKey("anonymous")
	if err != nil {
		t.Fatal(err)
	}
	pool := &worker.Pool{Store: s, Renderer: fakeRenderer{},
		ArtifactsDir: filepath.Join(dir, "artifacts"), TemplatesDir: filepath.Join("..", "..", "templates")}
	srv := &Server{
		Store: s, Renderer: fakeRenderer{}, Pool: pool,
		TemplatesDir:  pool.TemplatesDir,
		ListTemplates: func() ([]string, error) { return []string{"invoice"}, nil },
		MaxAttempts:   3,
		NoAuth:        true,
		AnonKey:       &store.APIKey{ID: anonID, Name: "anonymous", Templates: "*"},
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/v1/jobs", "application/json", strings.NewReader(`{"template":"invoice"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("no-auth mode should accept unauthenticated jobs, got %d", resp.StatusCode)
	}
	jobs, err := s.ListJobs(anonID, "", "", 0)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("job should be attributed to the anonymous key: %+v, %v", jobs, err)
	}
}
