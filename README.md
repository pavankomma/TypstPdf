# contract-notes-pipeline

A miniature, runnable homage to Zerodha's [*1.5 million PDFs in 25 minutes*](https://zerodha.tech/blog/1-5-million-pdfs-in-25-minutes/) — the nightly workflow that generates, digitally signs, and emails a contract note to every customer who traded that day.

This sample reproduces the **architecture** at toy scale: a Redis-brokered job queue, per-stage worker pools, Typst PDF generation, a signing stage, an email stage, and an S3-style object store as the handoff layer between stages.

```
CSV trades ──▶ [generate] Typst → PDF ──▶ [sign] ──▶ [email] .eml outbox
                    │                        │            │
                    └──── object store (0-9 prefix partitioned) ────┘
                              ▲ Redis carries the jobs between stages
```

## How each piece maps to the real stack

| This sample | Zerodha production |
|---|---|
| `internal/queue` — Redis list per stage, JSON jobs | [Tasqueue](https://github.com/kalbhor/tasqueue) + Redis broker/state |
| Goroutine worker pools in one process | ~40 ephemeral EC2 instances scheduled by Nomad, provisioned by Terraform, driven by Rundeck |
| `internal/gen` — `typst compile` per client in a temp dir | Typst (after outgrowing HTML→Chrome, then LaTeX) |
| `internal/store` — local dirs `0/`–`9/` sharding keys | S3 with ten fixed key prefixes to 10× the per-prefix rate limits |
| `internal/sign` — SHA-256 trailer appended after `%%EOF` | [jpdfsigner](https://github.com/zerodha/jpdfsigner): Java + OpenPDF HTTP service doing real PKCS#7 signatures |
| `internal/mail` — RFC 5322 `.eml` files in an outbox dir | Self-hosted Haraka SMTP cluster + [smtppool](https://github.com/knadh/smtppool) |

## Prerequisites

- Go 1.22+
- [Typst](https://github.com/typst/typst/releases) on `$PATH`
- Redis running locally (`redis-server --daemonize yes`)

## Run it

```sh
# 1. Fabricate the "exchange files": 200 clients, ~1.2k trades
go run ./cmd/pipeline seed -clients 200

# 2. Run the pipeline: 8 generate / 4 sign / 4 email workers
go run ./cmd/pipeline run -gen 8 -sign 4 -mail 4
```

Output lands in:

- `out/objectstore/<0-9>/pdfs/` — generated contract notes
- `out/objectstore/<0-9>/signed/` — "signed" copies (digest trailer)
- `out/outbox/*.eml` — MIME emails with the signed PDF attached

On a modest machine 200 notes take ~30s end to end; scale `-clients` and worker counts to taste. The generate pool dominates CPU, which is exactly why Zerodha gives PDF generation its own big-instance worker pool.

## Things worth reading in the code

- `templates/contract_note.typ` — the whole document is ~100 lines of Typst; compare with what the same table would cost in LaTeX or headless Chrome.
- `internal/store/store.go` — the prefix-partitioning trick, in ten lines.
- `cmd/pipeline/main.go` — the `pool()` helper: each stage drains its own queue and enqueues into the next, so stages overlap instead of running serially.

## Not production

The signer is a mock (real signing needs a certificate and PKCS#7), emails stop at the outbox, there are no retries/idempotency keys, and Redis isn't clustered. Each of those has a clear seam in the code where the real thing would plug in.
