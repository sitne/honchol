# design

Notes on why honchol looks the way it does. (This is a cleaned-up version of the design doc used during development.)

## Goals

1. **Honcho's model, minus the infrastructure.** Keep the peer → messages → conclusions → cards →
   context pipeline; replace the deployment with one binary and one file.
2. **Judgment where possible, generation where needed.** Most memory maintenance is *classification*
   (duplicate? contradiction? stable fact?) — small typed-judgment calls, not free text. Cloud LLM is
   reserved for extraction and synthesis, where generation is unavoidable.
3. **Cheap by construction.** A free-tier judge + a cheap LLM option; the semantic tier is an
   optional local ONNX sidecar (added back in v0.3 as an arm, not a dependency) — no vector DB,
   no GPU requirement on the serving box.
4. **Degrade, don't die.** Every external dependency (judge, LLM) can fail at any time; the store
   keeps accepting messages, and retrieval falls back to raw candidate order.

## Data model (SQLite, one file)

- `workspaces`, `peers`, `sessions`, `messages` — the conversation substrate (messages carry
  `processed` state for the derive pipeline).
- `conclusions` (+ `sources`) — derived facts with provenance (session, source message ids).
- `cards` — per-peer stable "about this peer" lines.
- `session_summaries` — condensed session history.
- FTS5 trigram indexes over message content and conclusion content.

## Derive pipeline (async, every 15 min via timer)

```
unprocessed messages ─▶ extract candidate facts (cloud LLM)
                            │
                            ▼
                judge: dedup / contradiction / kind   (typed choices)
                            │
                            ▼
          conclusions (+sources) ─▶ cards / session summaries
```

- Runs in a single transaction with a file lock; a crashed run leaves no partial state.
- Failures increment `tries`; after N tries the batch is dead-lettered and surfaced in
  `doctor` and `/health` (`degraded`), so nothing is silently lost.
- If the judge is down: dedup falls back to hash/near-text matching, kind falls back to `explicit`.

## Retrieval (two arms, RRF, then judge)

```
                 ┌─ FTS5(trigram) + LIKE candidates ─────────┐
query ──▶(JA→EN translation, optional)──▶                    │ RRF fuse ─▶ judge re-rank ─▶ top-K
                 └─ embedding arm (ONNX sidecar, optional) ──┘
   thin? LLM keyword expansion into the FTS arm / judge unavailable: fail-open
```

- Rationale: for a personal memory store, a literal candidate tier plus a relevance judge covers
  most queries and removes an entire component class (vector DB) — but real queries sometimes
  phrase facts differently from how they were stored, so v0.3 adds an *optional* local embedding
  arm instead of a mandatory semantic layer. See "v0.3 update" below.
- The expansion step exists because users phrase queries differently from how facts were stored
  ("calibration" vs "temperature scaling") — the LLM generates synonyms/bilingual variants, the
  FTS tier re-queries.

### v0.3 update — an optional local semantic tier

- One small ONNX embedding model (bekko, ~8M params, 384-dim, MIT) served by a local Python
  sidecar; no GPU, no vector DB — brute-force cosine over the conclusion set (tens of ms at ~44k
  rows). The two arms run independently over the full set and fuse via reciprocal-rank fusion
  (RRF); the judge re-ranks the fused top-N.
- Japanese queries are translated (JA→EN) by a second small ONNX model (LFM2-350M) before
  embedding; this recovered recall to human-translated-English levels in our bake-off.
- Everything stays fail-open and optional: sidecar down → literal arm alone;
  `HONCHO_LITE_EMBED_PROVIDER=none` → exact v0.1 behavior. The provider abstraction covers
  `none | local | api`.

## Judge abstraction

Wire formats, one interface:

- `kind=clef` — Cloudflare Workers AI envelope (`result.answers`, probabilities). Default:
  `clef-flash` on the free tier. Batched at ≤48 questions per call (the API rejects larger batches).
- `kind=sysone` — any local shim returning plain `{"answers": {...}}` (a small local judgment model
  speaking this protocol; the label is historical). No auth.
- `kind=mbja` (experimental) — a local ONNX scorer behind `/judge`
  (`{"query","texts"} → {"scores":[float]}`). Our zero-shot probes were not reliable enough to
  replace the defaults; kept for experimentation with fine-tuned relevance heads.

Judgments are typed: `choice` ({"related","weak","unrelated"} etc.), `noul` (claim truth), `score`
(numeric). The default engine for a given judgment is a config choice, not code.

## Deployment (systemd)

```
honchol-serve.service        loopback HTTP, receive-first
honchol-sidecar.service      loopback 8793: embeddings + JA→EN translation (v0.3, optional)
honchol-judge.service        loopback 8795: local ONNX scorer (experimental, optional)
honchol-derive.timer         15 min derive pass (oneshot, 30-min timeout)
honchol-backup.timer         daily .backup + integrity_check, 14 generations
```

- Loopback bind only; non-loopback requires an explicit env opt-in (`HONCHO_LITE_ALLOW_NON_LOOPBACK=1`).
- `/health` reports `degraded` for dead-letter backlog or a stale derive (last successful run too old).
- Migration from an existing Honcho deployment: import the export DB, then switch the client's
  `base_url` in one line (`deploy/switch_honcho.sh` does this atomically with a health preflight
  and a one-command rollback).

## Measured behavior (reference deployment)

- ~23k messages / ~44k conclusions imported in ≈16 min (single-threaded, batch inserts).
- Conclusion queries p50 ≈ 18 ms; search with judge p50 ≈ 1 s; chat synthesis ≈ 7–12 s.
- CPU-only (4 cores, no GPU) — the judge is 200–500 ms per batch on the free tier.
- v0.3 hybrid: JA→EN translation ≈0.2–0.4 s warm + embedding lookup <50 ms at ~44k rows;
  end-to-end latency stayed comparable to the literal tier in A/B runs.

## Future work

- Archive layer: bulk-ingest old exports (tweets, chat histories) without deriving them.
- Memory-judgment small model: serve the judge locally as an ONNX model, keeping the same `sysone`
  interface (first experiments with a fine-tuned encoder — `kind=mbja` — are in the tree but are
  not yet a replacement).
