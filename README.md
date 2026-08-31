# TypstPdf

An enterprise-ready PDF-generation service powered by [Typst](https://typst.app):
an authenticated HTTP API, a SQLite-backed render queue with retries, durable
signed artifacts, a full audit trail, and Prometheus metrics — in one Go binary.

```
POST /v1/jobs {template, data}  ──▶  SQLite queue  ──▶  typst compile  ──▶  signed PDF on disk
                                                                            + audited job row
```

Templates are plain `.typ` files in `templates/`; adding a document type means
dropping in one file — no code changes.

## Prerequisites

- Go 1.24+
- [Typst](https://github.com/typst/typst/releases) on `PATH` (Windows: `winget install Typst.Typst`)

## Quick start

```sh
# mint an API key (printed once; only its sha256 is stored)
go run ./cmd/server keys create myapp

# start the service
go run ./cmd/server

# queue a render
curl -s -H "Authorization: Bearer tp_..." -X POST \
  -d '{"template":"invoice","data":'"$(cat examples/invoice.json)"'}' \
  http://localhost:8080/v1/jobs
# → 202 {"id":"6f25ac...","status":"queued",...}

# poll, then download
curl -s -H "Authorization: Bearer tp_..." http://localhost:8080/v1/jobs/<id>
curl -s -H "Authorization: Bearer tp_..." http://localhost:8080/v1/jobs/<id>/pdf -o out.pdf
```

Everything lives under `data/` by default: `typstpdf.db` (SQLite, WAL),
`artifacts/` (the PDFs), `signing.key` (Ed25519 seed, created on first boot).

## API

Bearer-key auth on everything except `/healthz` and `/metrics`.

| Endpoint | |
|---|---|
| `POST /v1/jobs` | `{template, data?, filename?}` → `202` with the job; renders async with retries |
| `GET /v1/jobs/{id}` | status + provenance (template version, sha256, signature); `?include=data` adds the payload |
| `GET /v1/jobs/{id}/pdf` | the artifact (`409` while queued/running, `410` if purged by retention) |
| `GET /v1/jobs?status=&template=&limit=` | the caller's jobs, newest first |
| `POST /render/{template}` | synchronous render for interactive callers — audited as a job row too (`?filename=x.pdf`) |
| `GET /templates` | template names + content-hash versions |
| `GET /v1/signing-key` | Ed25519 public key for verifying artifact signatures |
| `GET /healthz`, `GET /metrics` | liveness / Prometheus (unauthenticated) |

Errors are JSON: `401` bad key, `403` template not in the key's allowlist,
`404` unknown template/job, `400` invalid body or name, `422` with typst's
diagnostics, `504` compile timeout. PDF responses carry `X-Job-Id`,
`X-Pdf-Sha256`, `X-Pdf-Signature`, `X-Pdf-Standard`, and `X-Template-Version`.

## API keys

```sh
go run ./cmd/server keys create billing -templates invoice,letter   # scoped key
go run ./cmd/server keys list
go run ./cmd/server keys disable billing
```

Keys are `tp_`-prefixed, shown once at creation, stored as sha256 only. Each
key can be restricted to specific templates; job reads are scoped to the key
that submitted them. `-no-auth` (dev only) attributes everything to an
`anonymous` key so the audit trail stays intact.

## The audit trail

Every render — async or sync — is a row in `jobs`: which key, which template
**at which content version** (sha256 of the `.typ` + defaults), the input's
sha256 (payload retained for re-render/dispute), timestamps, attempts, the
outcome, and the artifact's sha256 + Ed25519 signature. "Which template
version produced this document, from what data, for whom" is answerable for
the life of the retention window.

## Reliability

- **Queue with retries**: failed renders retry with exponential backoff
  (default 3 attempts, `-max-attempts`); the final failure keeps typst's
  diagnostics on the job row.
- **Crash recovery**: jobs stranded `running` by a crash are requeued at boot.
- **Atomic artifacts**: written via temp+rename; validated (`%PDF-` magic)
  before the job completes.
- **Retention**: an hourly janitor deletes finished jobs + artifacts older
  than `-retention` (default 720h; `0` keeps forever).
- **Graceful shutdown**: Ctrl-C stops intake, lets in-flight renders finish.

## Rendering guarantees

- **Missing-key resilience**: each template ships a `<name>.defaults.json`;
  request bodies deep-merge over it (objects recurse, nulls skipped), so a
  partial payload renders placeholders instead of hard-erroring the compile.
- **Deterministic fonts**: `--ignore-system-fonts` + the pinned `fonts/` dir
  (vendors DejaVu Sans; Typst's embedded fonts remain). Same request, same
  PDF, every host.
- **Archival PDF/A**: `-pdf-standard a-2b` enforces conformance, falling back
  loudly to a baseline PDF (logged + recorded on the job) so a document that
  must ship still ships.
- **Isolation with shared partials**: each render stages a copy of the
  templates tree in its own scratch dir, so concurrent requests can't see
  each other's data and typst's root confines file reads — while templates
  can still `#import "components/..."` shared partials (canopy's convention;
  `components/page.typ` ships the data-driven header/footer). Component
  edits feed the template version hash, so provenance stays honest. Template
  names are allowlist-validated; a typst crash kills one child process, not
  the server.

## Flags

| Flag | Default | |
|---|---|---|
| `-addr` | `:8080` | listen address |
| `-db` | `data/typstpdf.db` | SQLite database |
| `-artifacts` | `data/artifacts` | rendered PDF directory |
| `-signing-key` | `data/signing.key` | Ed25519 seed (empty = signing off) |
| `-templates` | `templates` | directory of `.typ` templates |
| `-fonts` | `fonts` | pinned fonts dir (empty = embedded fonts only) |
| `-pdf-standard` | *(none)* | e.g. `a-2b` for archival PDF/A-2b |
| `-concurrency` | NumCPU | queue workers / concurrent typst compiles |
| `-timeout` | `30s` | per-compile timeout |
| `-max-attempts` | `3` | render attempts per job |
| `-retention` | `720h` | purge finished jobs + artifacts after this (`0` = never) |
| `-no-auth` | `false` | DEV ONLY: skip auth, act as the `anonymous` key |
| `-tls-cert` / `-tls-key` | | serve HTTPS when both set |
| `-typst` | `typst` | path to the typst executable |
| `-system-fonts` | `false` | allow host fonts (breaks determinism) |

## Observability

Structured JSON logs (`log/slog`) and Prometheus metrics:
`typstpdf_jobs_total{status}`, `typstpdf_render_duration_seconds{template}`,
`typstpdf_queue_depth`, `typstpdf_archival_fallback_total`,
`typstpdf_http_request_duration_seconds{method,route,status}`.

## Template designer

A Vue 3 template manager (built into the binary, served at `/designer/`) for
designing templates against the **real** pipeline — defaults merge, pinned
fonts, PDF standard and all, so the preview is byte-for-byte what production
renders:

```sh
go run ./cmd/server -designer -no-auth     # then open http://localhost:8080/designer/
```

- Browse/create/delete templates; edit source, defaults, and example payload
  in a CodeMirror editor with Typst syntax highlighting and intellisense —
  built-in snippets plus payload-key completion for `d.` from the template's
  own defaults/example.
- Live inline preview (per-page SVG, Mermaid-Live-style: drag to pan,
  Ctrl+wheel or the floating widget to zoom); PDF view on demand. Render
  stats (pages · size · time), SVG/PDF export, draggable 60/40 split,
  collapsible rail.
- Compile errors show inline with a "Go to line" jump into the editor; a
  key-audit chip flags `d.` keys missing from the defaults with a one-click
  placeholder fix. Ctrl+S saves, Ctrl+Enter renders; switching templates
  with unsaved edits asks first.
- **Page setup tab**: header/footer, page numbers, paper, and margins as a
  form. It edits a `page` object in the template's defaults, consumed by the
  shared `templates/components/page.typ` partial (`#show: branded.with(d)` —
  one click applies it to a template's source; new templates scaffold with
  it). Header/footer are payload data: defaultable per template, overridable
  per request.
- Preview against the example payload or defaults-only (the placeholder
  render the test suite enforces).
- **Save is validated**: a template that doesn't compile against its defaults
  is rejected with diagnostics — the same guarantee CI enforces, applied at
  save time. Saves return the new content-hash version.
- A Jobs page shows the live queue with statuses, versions, and PDF links.

The designer is off by default and must be enabled with `-designer` — its
preview endpoint compiles arbitrary Typst source, so enable it in trusted
environments only. With auth on, the UI prompts for a `tp_…` key (top-right).

UI development: `cd web/designer && npm install && npm run dev` (Vite on
:5173, proxying to the Go server on :8080). `npm run build` regenerates
`web/designer/dist/`, which `go:embed` compiles into the binary — `dist/` is
committed so node-less machines still build. The design system
(`src/styles/doclerk.css` + vendored Nunito/JetBrains Mono) is shared with
the EasyScan project: design tokens in three tiers (raw values → tonal ramps
→ semantic aliases), global class-based styling, no CSS in components.

## Templates

Bundled: `invoice`, `report`, `letter`, `contract_note`, `certificate`,
`filing` — each with a sample payload in `examples/` and a placeholder set in
`templates/<name>.defaults.json`. To add your own: write `templates/mydoc.typ`
starting with `#let d = json("data.json")`, add `mydoc.defaults.json` covering
every key it reads, and an `examples/mydoc.json` — the test suite renders
every template with both its example and an empty body, so a missing default
fails in CI, not production.

## Tests

```sh
go test ./...
```

Store (queue lifecycle, retries, recovery, purge, key scoping), signing
(roundtrip, tamper detection), worker (retries, artifact + signature,
version stamping — via a fake renderer, no typst needed), API (auth,
allowlists, job lifecycle over HTTP, audit rows), and the render suite
(every template × example/empty payloads, PDF/A, path guards — skips when
typst is absent).

## Lineage

Started as a toy reproduction of Zerodha's
[*1.5 million PDFs in 25 minutes*](https://zerodha.tech/blog/1-5-million-pdfs-in-25-minutes/)
pipeline; the contract-note template survives from that era. The current
architecture — queue → render → validate → sign → store → audited metadata —
is patterned after the notice-generation pipeline in the canopy project,
scaled to a single binary with SQLite as the system of record.
