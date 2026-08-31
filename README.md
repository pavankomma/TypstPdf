# TypstPdf

An HTTP PDF-generation service powered by [Typst](https://typst.app). POST a JSON
document, pick a template, get a PDF back.

```
POST /render/invoice  { ...json... }  ──▶  typst compile  ──▶  application/pdf
```

Templates are plain `.typ` files in `templates/`; adding a document type means
dropping in one file — no code changes.

## Prerequisites

- Go 1.22+
- [Typst](https://github.com/typst/typst/releases) on `PATH` (Windows: `winget install Typst.Typst`)

## Run it

```sh
go run ./cmd/server
```

Flags:

| Flag | Default | |
|---|---|---|
| `-addr` | `:8080` | listen address |
| `-templates` | `templates` | directory of `.typ` templates |
| `-typst` | `typst` | path to the typst executable |
| `-fonts` | `fonts` | pinned fonts dir (empty = typst's embedded fonts only) |
| `-system-fonts` | `false` | allow host system fonts (breaks render determinism) |
| `-pdf-standard` | *(none)* | PDF standard to enforce, e.g. `a-2b` for archival PDF/A-2b |
| `-concurrency` | NumCPU | max concurrent typst compiles |
| `-timeout` | `30s` | per-compile timeout |

## API

| Endpoint | |
|---|---|
| `GET /healthz` | service + typst version check |
| `GET /templates` | lists available template names |
| `POST /render/{template}` | JSON body in, PDF out (`?filename=x.pdf` to name the download) |

```sh
curl -s http://localhost:8080/templates
# {"templates":["certificate","contract_note","filing","invoice","letter","report"]}

curl -s -X POST --data-binary @examples/invoice.json \
  http://localhost:8080/render/invoice -o invoice.pdf
```

Errors come back as JSON: `404` unknown template, `400` invalid JSON body or
template name, `422` with typst's diagnostics when the data doesn't fit the
template, `504` on compile timeout. Successful responses carry an
`X-Pdf-Sha256` header, and `X-Pdf-Standard` when a PDF standard is configured
(`baseline-fallback` if the document failed conformance and the baseline
re-render was served — the fallback is also logged loudly).

## Templates

Each template is a self-contained Typst file that reads its input with
`json("data.json")` — the service writes the request body next to a copy of the
template in a scratch dir and runs `typst compile` there. Bundled templates,
each with a matching sample payload in `examples/`:

- `invoice` — line items, totals, tax, notes
- `report` — title block, metric tiles, sections
- `letter` — formal letter with sender/recipient blocks
- `contract_note` — equity trade contract note (Indian-broker style)
- `certificate` — formal certificate (completion, membership, …)
- `filing` — structured legal document (articles, bylaws, agreements)

To add your own: write `templates/mydoc.typ` starting with
`#let d = json("data.json")` and it is immediately available at
`POST /render/mydoc`. Money/number fields are passed pre-formatted strings so
the caller controls locale and grouping.

### Defaults: missing keys can't fail a render

Typst **hard-errors on a missing dictionary key**, so a caller omitting one
field a template touches would otherwise fail the whole compile. Each bundled
template ships a `templates/<name>.defaults.json` holding a placeholder (`—`)
for every key it reads. The request body is deep-merged over it before
rendering:

- objects merge recursively (a partial `{"seller":{"name":"Acme"}}` keeps the
  placeholder address),
- arrays and scalars from the request replace the default,
- JSON `null`s in the request are **skipped**, so a null can never clobber a
  placeholder the template expects to be a string.

Worst case a PDF renders with placeholders instead of the request 422-ing.
When you add a template, add its defaults file too — the test suite renders
every template with an empty `{}` body, so a template that reads a key its
defaults don't supply fails in CI, not in production. Array *elements* are the
caller's responsibility (defaults can't reach inside a replaced array).

### Deterministic fonts

Compiles run with `--ignore-system-fonts` plus the pinned `fonts/` directory,
so the same request produces the same PDF on every host (including slim
containers with no fonts installed). Typst's embedded fonts (Libertinus, New
Computer Modern, DejaVu Sans Mono) stay available; `fonts/` vendors DejaVu
Sans (Bitstream Vera license, see `fonts/LICENSE-DejaVu.txt`) for the
contract-note template. Put any other family a template names into `fonts/`.
`-system-fonts` restores the old host-dependent behavior if you need it.

### Archival PDF/A

Run with `-pdf-standard a-2b` to enforce archival PDF/A-2b conformance
(retention requirements — e.g. SEBI/IRS records schedules). If a document
fails conformance, the service re-renders it as a baseline PDF, serves that,
and logs an `ARCHIVAL FALLBACK` line — a document that must ship still ships,
and the archival gap becomes an ops signal instead of a render failure.
Any typst-supported standard works (`1.4`–`2.0`, `a-1b`…`a-4e`, `ua-1`).

## Tests

```sh
go test ./...
```

Unit tests pin the template-name guard, the defaults merge (including the
null-skip), and the output validation; integration tests drive every template
through the real typst binary with both its example payload and an empty body,
plus a PDF/A-2b conformance render. The integration tests skip automatically
when `typst` is not on `PATH`.

## Design notes

- `internal/render` bounds concurrent `typst compile` processes with a
  semaphore (`-concurrency`), since compiles are CPU-bound. Process-per-render
  also isolates crashes: a typst OOM kills one child process, not the server.
- Each render runs in its own temp dir and is cleaned up afterwards, so
  concurrent requests can't see each other's data; typst's project root is the
  scratch dir, so templates can't read arbitrary host files.
- Template names are validated against the allowlist `[A-Za-z0-9._-]+` (plus
  an explicit `..` check), so requests can't escape the templates directory on
  any OS. The guard is a pure, unit-tested function.
- Rendered bytes are checked for the `%PDF-` magic header before being
  returned; the sha256 rides the `X-Pdf-Sha256` response header.

## Lineage

This repo started as a toy reproduction of Zerodha's
[*1.5 million PDFs in 25 minutes*](https://zerodha.tech/blog/1-5-million-pdfs-in-25-minutes/)
pipeline (Redis queue, signing and email stages). It has been repurposed into a
general-purpose Typst rendering service; the contract-note template survives
from that era. The hardening measures (defaults merge, per-template render
tests, pinned fonts, PDF/A with loud fallback, output validation) are patterned
after the notice-generation pipeline in the canopy project.
