# honchol

**English** | [日本語](README.ja.md)

**A lightweight, self-hosted, Honcho-compatible memory server — one Go binary + one SQLite file.**

honchol implements a practical subset of the [Honcho](https://github.com/plastic-labs/honcho) v3 HTTP API,
so existing clients (honcho-ai SDKs, agent memory plugins) can point at it by changing a single `base_url`.
No Docker, no Postgres — a single static binary and a single database file, small enough to run on a
CPU-only box next to the agent it serves. An optional local Python sidecar (v0.3) adds a hybrid
retrieval tier — ONNX embeddings + query translation — when the literal tier isn't enough.

> Unofficial project — not affiliated with Plastic Labs. "Honcho" refers to the upstream open-source
> project whose API this server is compatible with.

[![CI](https://github.com/sitne/honchol/actions/workflows/ci.yml/badge.svg)](https://github.com/sitne/honchol/actions/workflows/ci.yml)

## Why

Upstream Honcho is a Docker/Postgres deployment. Small setups — a person running a single agent on a
home server or a cheap VPS — don't need a database server to keep memory. honchol fills that gap.

Design goals:

- **Receive-first**: incoming messages are persisted before anything else; derivation is async and retryable.
- **No vector store required**: retrieval starts as FTS candidates + an LLM judge; an optional local embedding arm (v0.3) fuses in via RRF — no ANN index, no external vector DB.
- **Fail-open everywhere**: if the judge or the LLM is unreachable, the server keeps serving (degraded, never down).
- **Portable**: one binary, one file. Backup = copy the file.

## Features

- **Honcho v3 HTTP subset** — workspaces, peers (cards, representation, context), sessions, messages,
  conclusions, search (workspace/session/peer), chat, queue status. Exercised against the honcho-ai
  Python SDK with live calls (see `server/smoke_sdk.py`).
- **SQLite storage** — WAL mode, pure-Go driver (`modernc.org/sqlite`), no cgo.
- **Hybrid retrieval** — two independent arms: FTS5 (trigram) + LIKE candidates, and (v0.3,
  optional) a local ONNX embedding arm; the arms fuse via reciprocal-rank fusion and an LLM
  judge re-ranks by relevance. Long queries are keyword-expanded when candidates are thin,
  and Japanese queries can be translated (JA→EN) before embedding.
- **Optional model sidecar** — a small Python service (`sidecar/`) serves bekko embeddings +
  LFM2-350M JA→EN translation over loopback HTTP, CPU-only, no torch. Without it, search
  falls back to the literal tier (fail-open).
- **Async derivation pipeline** — unprocessed messages → fact extraction (cloud LLM) →
  dedup/contradiction/kind judgments → conclusions with sources → cards and session summaries.
  Single-flight, retries with dead-lettering.
- **Pluggable judge** — `clef` (Cloudflare Workers AI free tier; default), any judgment shim
  speaking the plain-answers protocol (`kind=sysone`; typically a small local model), or an
  experimental local ONNX scorer (`kind=mbja`). The LLM is used for generation only, and any
  OpenAI-compatible endpoint works.
- **systemd deployment included** — serve unit + 15-minute derive timer + daily integrity-checked
  backups (`deploy/`).

## Quickstart

```bash
git clone https://github.com/sitne/honchol && cd honchol/server
go build -o honchol .
./honchol serve -addr 127.0.0.1:8792 -db honchol.db
```

Set what honchol needs (env vars; secrets are referenced by variable *name*, never stored in config):

```bash
export OPENCODE_GO_API_KEY=...            # LLM key — or point HONCHO_LITE_LLM_* at any OpenAI-compatible endpoint
export CLOUDFLARE_API_TOKEN=... CLOUDFLARE_ACCOUNT_ID=...   # optional: enables the judge
```

Smoke-test with the real SDK:

```bash
pip install honcho-ai
python smoke_sdk.py
```

For a systemd deployment (serve + derive timer + backups), see [`deploy/README.md`](deploy/README.md).
Design rationale: [`docs/DESIGN.md`](docs/DESIGN.md).

Hybrid retrieval (optional) is served by the Python sidecar in `sidecar/` — CPU-only ONNX; the
server picks it up via `HONCHO_LITE_EMBED_PROVIDER=local` + `HONCHO_LITE_FUSE=1` (see
`deploy/honchol.env` for a ready-made block).

## Configuration (environment variables)

| Variable | Default | Purpose |
|---|---|---|
| `HONCHO_LITE_LLM_BASE_URL` | `https://opencode.ai/zen/go/v1` | OpenAI-compatible base URL for extraction/synthesis |
| `HONCHO_LITE_LLM_MODEL` | `deepseek-v4.1-flash` | model id |
| `HONCHO_LITE_LLM_KEY_ENV` | `OPENCODE_GO_API_KEY` | name of the env var holding the API key |
| `HONCHO_LITE_LLM_SESSION` | `honcho-lite` | stable session header (some gateways require one) |
| `HONCHO_LITE_JUDGE_URL` | — | judge endpoint (clef envelope or sysone plain) |
| `HONCHO_LITE_JUDGE_KIND` | `clef` | `clef` \| `sysone` |
| `HONCHO_LITE_JUDGE_MODEL` | `clef-flash` | sent only when `kind=clef` |
| `HONCHO_LITE_JUDGE_KEY_ENV` | `CLOUDFLARE_API_TOKEN` | key env name; empty string = no auth |
| `HONCHO_LITE_JUDGE_FALLBACK_URL` | — | tried when the primary judge fails |
| `HONCHO_LITE_SEARCH_JUDGE` / `_EXPAND` / `_CANDIDATE_CAP` | on / on / 40 | two-stage search knobs |
| `HONCHO_LITE_EMBED_PROVIDER` | `none` | `none` \| `local` (sidecar) \| `api` — enables the embedding arm |
| `HONCHO_LITE_EMBED_URL` / `_DIM` / `_TIMEOUT_MS` | — / — / 5000 | `/embed` endpoint settings |
| `HONCHO_LITE_FUSE` | `0` | `1` = RRF fusion of the FTS + embedding arms |
| `HONCHO_LITE_TRANSLATE_PROVIDER` | `none` | `none` \| `local` — JA→EN query translation before embedding |
| `HONCHO_LITE_TRANSLATE_URL` / `_TIMEOUT_MS` | — / 2500 | translation-stage settings |
| `HONCHO_LITE_QUERY_DEADLINE_MS` | 10000 | overall deadline for one ranked search |
| `HONCHO_LITE_DERIVE_*` | see `server/README.md` | derive pipeline knobs |
| `HONCHO_LITE_*_TIMEOUT_MS` (LLM / judge / search) | 90000 / 20000 / 15000 | call timeouts |
| `HONCHO_LITE_MAX_BODY_BYTES` | 4194304 (4 MiB) | request-body cap |
| `HONCHO_LITE_ALLOW_NON_LOOPBACK` | — | required to bind non-loopback (no auth otherwise) |

## Performance notes

Measured on a CPU-only container (4 cores, no GPU), database ≈ 23k messages / 44k conclusions (~60 MB):

- conclusion queries: p50 ≈ 18 ms
- search (candidates + judge re-rank): p50 ≈ 1 s
- chat synthesis (one cloud LLM round-trip): ≈ 7–12 s
- hybrid search adds a local JA→EN translation (≈0.2–0.4 s warm on CPU) and an embedding
  lookup (<50 ms at ~44k conclusions) ahead of fusion — end-to-end latency stayed
  comparable to the literal tier in A/B runs.

Caveat: the only comparison to upstream here is qualitative (chat/dialectic completed where the
upstream deployment we migrated from timed out on the same data), and upstream uses semantic search
while honchol uses literal candidates + judge re-ranking — different machinery, so treat these as
deployment notes, not a benchmark.

## Scope / non-goals (v0.3.1)

- The embedding tier is optional and local (ONNX sidecar); no managed vector DB, no ANN index —
  brute-force cosine over the conclusion set is enough at personal scale.
- No authentication: the server and sidecars are meant to run loopback-only next to the client.
- Single-user scale: the derive pipeline is one pass every 15 minutes, not a multi-tenant worker fleet.
- Linux/macOS builds for now (single-flight locking uses flock; on Windows, use WSL).

## License

MIT — see [LICENSE](LICENSE). This is an independent implementation (no code from upstream Honcho);
API compatibility is intentional for client interoperability.
