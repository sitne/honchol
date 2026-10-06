# design

Notes on why honchol looks the way it does. (This is a cleaned-up version of the design doc used during development.)

## Goals

1. **Honcho's model, minus the infrastructure.** Keep the peer → messages → conclusions → cards →
   context pipeline; replace the deployment with one binary and one file.
2. **Judgment where possible, generation where needed.** Most memory maintenance is *classification*
   (duplicate? contradiction? stable fact?) — small typed-judgment calls, not free text. Cloud LLM is
   reserved for extraction and synthesis, where generation is unavoidable.
3. **Cheap by construction.** A free-tier judge + a cheap LLM option; no embeddings, no vector DB,
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

## Retrieval (two stages, no embeddings)

```
query ─▶ SQLite FTS5(trigram) + LIKE candidates ─▶ (thin? LLM keyword expansion) ─▶ judge re-rank ─▶ top-K
             │
             └─ judge unavailable: fail-open, raw candidate order
```

- Rationale: for a personal memory store, a literal candidate tier plus a relevance judge covers
  most queries, and it removes an entire component (embedding model + vector index + drift).
- The expansion step exists because users phrase queries differently from how facts were stored
  ("calibration" vs "temperature scaling") — the LLM generates synonyms/bilingual variants, the
  FTS tier re-queries.
- If this proves insufficient in practice, embeddings are the planned last resort — one component
  to add back, not a redesign.

## Judge abstraction

Two wire formats, one interface:

- `kind=clef` — Cloudflare Workers AI envelope (`result.answers`, probabilities). Default:
  `clef-flash` on the free tier. Batched at ≤48 questions per call (the API rejects larger batches).
- `kind=sysone` — any local shim returning plain `{"answers": {...}}` (a small local judgment model
  speaking this protocol; the label is historical). No auth.

Judgments are typed: `choice` ({"related","weak","unrelated"} etc.), `noul` (claim truth), `score`
(numeric). The default engine for a given judgment is a config choice, not code.

## Deployment (systemd)

```
honchol-serve.service        loopback HTTP, receive-first
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

## Future work

- Semantic tier as an optional add-on (last resort, only if literal+judge proves insufficient).
- Archive layer: bulk-ingest old exports (tweets, chat histories) without deriving them.
- Memory-judgment small model: serve the judge locally as an ONNX model, keeping the same `sysone` interface.
