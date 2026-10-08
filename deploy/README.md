# Deploy (systemd)

Assumed layout: `/opt/honchol/{server,sidecar,deploy,data,venv}` — adjust the paths inside the
unit files if you installed somewhere else.

## Prerequisites

- root (unit installation), systemd
- a Go toolchain to build the binary
- `sqlite3` CLI (used by the daily backup for `.backup` + `PRAGMA integrity_check`; e.g. `apt install sqlite3`)
- `python3` + venv — optional, only for the model sidecar (embedding arm / translation stage).
  The server itself runs without it (search degrades to the literal tier).

## Units

- `honchol-serve.service` — serves the HTTP API on `127.0.0.1:8792` (the baseUrl target).
- `honchol-sidecar.service` — `127.0.0.1:8793`: bekko embeddings + LFM2 translation (v0.3 embedding arm).
- `honchol-judge.service` — `127.0.0.1:8795`: optional local ONNX relevance judge (experimental;
  installed but not enabled by default).
- `honchol-derive.timer` — every 15 min: unprocessed messages → conclusions (`TimeoutStartSec=1800`).
- `honchol-backup.timer` — daily 04:10: online backup into `data/backups/`, 14 generations, integrity-checked.
- env handling: `run_serve.sh` / `run_derive.sh` source `/etc/honchol/env` first, then
  `deploy/honchol.env` (derived settings). Missing `CLOUDFLARE_ACCOUNT_ID` simply disables
  the judge — the server starts anyway and search degrades fail-open.

## Install (first time)

    sudo bash deploy/install.sh        # lays out /opt/honchol, builds, creates venv, installs+enables units
    sudoedit /etc/honchol/env          # fill in OPENCODE_GO_API_KEY (+ CLOUDFLARE_* for the judge)
    python3 /opt/honchol/sidecar/download_lfm2.py   # optional: fetch the translation model (~375 MB)
    systemctl start honchol-serve honchol-sidecar honchol-derive.timer honchol-backup.timer
    curl -s http://127.0.0.1:8792/health

The installer seeds `/etc/honchol/env` (mode 600), creates `/opt/honchol/venv` and pip-installs
`sidecar/requirements.txt` (best effort). The bekko embedding model downloads automatically on
first sidecar start (pinned revision).

## Hybrid search (v0.3)

- Env (see `deploy/honchol.env`): `HONCHO_LITE_EMBED_PROVIDER=local` + `HONCHO_LITE_EMBED_URL`
  (sidecar), `HONCHO_LITE_FUSE=1` (RRF fusion of the FTS + embedding arms),
  `HONCHO_LITE_TRANSLATE_PROVIDER=local` (JA→EN query translation before the embedding arm).
- Rollback to the literal tier: set `HONCHO_LITE_EMBED_PROVIDER=none` / `HONCHO_LITE_FUSE=0` /
  `HONCHO_LITE_TRANSLATE_PROVIDER=none`, then `systemctl restart honchol-serve` (env is read at
  startup). Fully stopping the sidecar (`systemctl stop honchol-sidecar`) is also safe — the
  server fails open.
- Backfill vectors for existing conclusions after enabling:
  `cd server && ./honchol embed-backfill -db ../data/honchol.db -batch 256` (resumable, flock-guarded).
  New conclusions are embedded automatically by the derive timer.

## Cutover from an existing Honcho (optional)

1. `deploy/backup.sh` (snapshot first)
2. `deploy/switch_honcho.sh status` (record current state)
3. `HONCHO_JSON=... deploy/switch_honcho.sh lite` (health preflight; `--force` overrides)
4. verify in a new client session + `journalctl -u honchol-serve -n 30 --no-pager`
5. watch for 24 h (chat/search feel, derive backlog, `/health` degraded flag)
6. rollback: `HONCHO_JSON=... deploy/switch_honcho.sh remote` (one command)

## Verify

- New client sessions show requests in `journalctl -u honchol-serve`.
- Your client logs show `base_url` pointing at `http://127.0.0.1:8792`.
- `curl -s http://127.0.0.1:8792/health` → `status=ok`
  (`degraded` → check `dead_letter` / `stale_derive` in the response).
- With the sidecar up: `curl -s http://127.0.0.1:8793/health` → `"loaded": true` for both models.

## Restore

    systemctl stop honchol-serve
    rm -f data/honchol.db-wal data/honchol.db-shm
    cp data/backups/honchol-<TS>.db data/honchol.db
    systemctl start honchol-serve
    cd server && ./honchol embed-backfill -db ../data/honchol.db -batch 256   # regenerate vectors if needed
    # verify: ./honchol doctor -db data/honchol.db   +   curl /health

## Known limitations (import path)

- `cards` / `summaries` are not carried over from an export database — cards rebuild
  from new messages via derive.
- Conclusion provenance (`session_id` / `level` / sources) is backfilled with fixed
  values where the source export lacks it.
- Imported messages are marked `processed=1` (they won't be re-derived).
- The literal tier (FTS5/trigram candidates + judge re-ranking) is the baseline; the v0.3
  embedding arm is optional and requires the sidecar.

## Security notes

- **No authentication.** The server binds to loopback by default; a non-loopback
  bind is refused unless `HONCHO_LITE_ALLOW_NON_LOOPBACK=1` is set explicitly.
  (An empty host like `:8792` counts as non-loopback.)
- The model sidecars are loopback-only as well (no auth; they guard Host/Origin and cap
  request sizes/concurrency).
- Keep `/etc/honchol/env` readable only by root (the installer creates it as 0600).
- Backups live on the same disk (single-disk); no external alerting built in — point any
  watchdog at `/health` (`status != "ok"`) and sidecar reachability, or watch the journal.
