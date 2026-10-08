# honchol — server

Go single-binary implementation of the Honcho v3 HTTP subset, backed by SQLite.
See the [root README](../README.md) for the overview and [`../deploy`](../deploy) for the
systemd deployment.

## Build / run

    go build -o honchol .
    ./honchol serve -addr 127.0.0.1:8792 -db honchol.db
    ./honchol doctor -db honchol.db          # store health + counts
    ./honchol doctor -db honchol.db -judge   # also probe the judge endpoint

## Test

    go vet ./... && go test ./...

SDK smoke test (needs `pip install honcho-ai` and a running server):

    ./honchol serve -addr 127.0.0.1:8792 -db /tmp/honchol-smoke.db &
    python smoke_sdk.py http://127.0.0.1:8792   # exercises the SDK against the live server

Tip: stop test servers by PID (`kill $!` right after starting, or locate it with
`pgrep -f 'addr 127.0.0.1:879'`). Avoid `pkill -x honchol`: it stops every honchol
process on the machine, including a production instance. `pkill -f 'honchol serve'`
can match the invoking shell itself.

Note: `8788` is the binary's built-in default address; the examples in this repo use
`8792` consistently (matching the systemd units under deploy/).

## Implemented API surface

- workspaces: ensure (`POST /v3/workspaces`)
- peers: create/get/update, peer context, representation (GET+POST, body/query)
- cards: get/put
- sessions: create/get/update/delete/list, peer sessions, session peers config
- messages: add (bare-array response), list (page shape), session context (token budget)
- search: workspace / session / peer — FTS5 candidates (+ optional embedding arm, RRF-fused in
  v0.3) + judge re-ranking; thin results retry once with LLM keyword expansion
- conclusions: create / list / query / delete
- queue/status: processed-message aggregation
- chat / dialectic: cloud-LLM synthesis over card + related conclusions + related messages → `{content}`

## Implementation notes

- Response shapes follow the honcho v3 strict models (no extra fields on the wire).
- `created_at` is normalized to UTC ISO (`2006-01-02T15:04:05.000000+00:00`).
- SQLite: WAL, single-writer connection, `busy_timeout=5000`.
- Receive-first: message writes commit before any derivation is attempted.

## Judge client

Judgments (choice / noul / score) are delegated to a judge endpoint, in one of three wire formats:

- `clef` — Cloudflare Workers AI envelope (`{"success":true,"result":{"answers":{...}}}`). Default model `clef-flash`.
- `sysone` — plain `{"answers":{...}}`, no auth (any local judgment shim).
- `mbja` — experimental: `POST {"query","texts"} → {"scores":[float]}` from a local ONNX scorer
  (`../sidecar/mbja_judge_server.py`). Not a replacement for the defaults — see the header note there.

Env (secrets are referenced by variable *name*):

    HONCHO_LITE_JUDGE_URL          (required to enable) full endpoint URL
    HONCHO_LITE_JUDGE_KIND         clef (default) | sysone | mbja
    HONCHO_LITE_JUDGE_MODEL        clef-flash (default; sent only for kind=clef)
    HONCHO_LITE_JUDGE_KEY_ENV      CLOUDFLARE_API_TOKEN (default; empty string = no auth)
    HONCHO_LITE_JUDGE_TIMEOUT_MS   20000 (default)
    HONCHO_LITE_JUDGE_FALLBACK_URL optional second endpoint used when primary fails

Check both primary and fallback with:

    ./honchol doctor -db honchol.db -judge

## Derive / search env knobs

    HONCHO_LITE_LLM_BASE_URL       https://opencode.ai/zen/go/v1 (default)
    HONCHO_LITE_LLM_MODEL          deepseek-v4.1-flash (default)
    HONCHO_LITE_LLM_KEY_ENV        OPENCODE_GO_API_KEY (default)
    HONCHO_LITE_LLM_SESSION        value for the x-opencode-session header (default honcho-lite)
    HONCHO_LITE_SEARCH_JUDGE       0 disables judge re-ranking
    HONCHO_LITE_SEARCH_EXPAND      0 disables query expansion
    HONCHO_LITE_SEARCH_CANDIDATE_CAP  40 (default)
    HONCHO_LITE_DERIVE_BATCH / _SESSIONS / _MAX_BATCHES / _QUIET_MIN / _SUMMARY_MIN_NEW / _MAX_TRIES
    HONCHO_LITE_DERIVE_MAX_CANDIDATES  8 (default)
    HONCHO_LITE_LLM_TIMEOUT_MS         90000 (LLM call timeout)
    HONCHO_LITE_JUDGE_TIMEOUT_MS       20000 (judge timeout; the fallback uses the same value)
    HONCHO_LITE_SEARCH_JUDGE_TIMEOUT_MS 15000 (judge timeout during search re-ranking)
    HONCHO_LITE_MAX_BODY_BYTES         4194304 (4 MiB request-body cap)
    HONCHO_LITE_FTS_REBUILD            0 skips the FTS rebuild at startup

## Hybrid retrieval (v0.3)

    HONCHO_LITE_EMBED_PROVIDER    none (default) | local (sidecar) | api (OpenAI-compatible /embeddings)
    HONCHO_LITE_EMBED_URL         e.g. http://127.0.0.1:8793/embed (local) or https://<api>/embeddings (api)
    HONCHO_LITE_EMBED_DIM         required for provider=api (local takes the sidecar-reported dim)
    HONCHO_LITE_EMBED_TIMEOUT_MS  5000
    HONCHO_LITE_FUSE              1 enables RRF fusion of the FTS + embedding arms (default 0)
    HONCHO_LITE_TRANSLATE_PROVIDER none (default) | local — JA→EN query translation before the embedding arm
    HONCHO_LITE_TRANSLATE_URL     http://127.0.0.1:8793/translate (default)
    HONCHO_LITE_TRANSLATE_TIMEOUT_MS 2500
    HONCHO_LITE_QUERY_DEADLINE_MS 10000 (overall deadline for one ranked search)

Backfill vectors for existing rows (resumable, single-flight):

    ./honchol embed-backfill -db honchol.db -batch 256

The embedding tier is served by `../sidecar/model_sidecar.py` (bekko ONNX embeddings + LFM2-350M
translation). Everything is fail-open: no sidecar, or `EMBED_PROVIDER=none` → literal tier only.

## Derive pipeline

    ./honchol derive -db db [-session ID] [-requeue-dead]

- Unprocessed messages → extraction (LLM) → judgments (dedup / contradiction / card-suitability)
  → conclusions (+sources) / card updates → `processed=1`; quiet sessions get summaries.
- Single transaction + file lock; failures `tries++` (dead-letter after `HONCHO_LITE_DERIVE_MAX_TRIES`, default 3).
- Idempotent: re-running after a crash resumes cleanly.
