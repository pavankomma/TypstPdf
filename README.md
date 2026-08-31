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
# {"templates":["contract_note","invoice","letter","report"]}

curl -s -X POST --data-binary @examples/invoice.json \
  http://localhost:8080/render/invoice -o invoice.pdf
```

Errors come back as JSON: `404` unknown template, `400` invalid JSON body,
`422` with typst's diagnostics when the data doesn't fit the template,
`504` on compile timeout.

## Templates

Each template is a self-contained Typst file that reads its input with
`json("data.json")` — the service writes the request body next to a copy of the
template in a scratch dir and runs `typst compile` there. Bundled templates,
each with a matching sample payload in `examples/`:

- `invoice` — line items, totals, tax, notes
- `report` — title block, metric tiles, sections
- `letter` — formal letter with sender/recipient blocks
- `contract_note` — equity trade contract note (Indian-broker style)

To add your own: write `templates/mydoc.typ` starting with
`#let d = json("data.json")` and it is immediately available at
`POST /render/mydoc`. Money/number fields are passed pre-formatted strings so
the caller controls locale and grouping.

## Design notes

- `internal/render` bounds concurrent `typst compile` processes with a
  semaphore (`-concurrency`), since compiles are CPU-bound.
- Each render runs in its own temp dir and is cleaned up afterwards, so
  concurrent requests can't see each other's data.
- Template names are validated against `[A-Za-z0-9._-]+`, so requests can't
  escape the templates directory.

## Lineage

This repo started as a toy reproduction of Zerodha's
[*1.5 million PDFs in 25 minutes*](https://zerodha.tech/blog/1-5-million-pdfs-in-25-minutes/)
pipeline (Redis queue, signing and email stages). It has been repurposed into a
general-purpose Typst rendering service; the contract-note template survives
from that era.
