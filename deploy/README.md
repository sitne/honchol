# Deploy (systemd)

Assumed layout: `/opt/honchol/{server,data,deploy}` — adjust the paths inside the
unit files if you installed somewhere else.

## Prerequisites

- root (unit installation), systemd
- a Go toolchain to build the binary
- `sqlite3` CLI (used by the daily backup for `.backup` + `PRAGMA integrity_check`; e.g. `apt install sqlite3`)

## Units

- `honchol-serve.service` — serves the HTTP API on `127.0.0.1:8792` (the baseUrl target).
- `honchol-derive.timer` — every 15 min: unprocessed messages → conclusions (`TimeoutStartSec=1800`).
- `honchol-backup.timer` — daily 04:10: online backup into `data/backups/`, 14 generations, integrity-checked.
- env handling: `run_serve.sh` / `run_derive.sh` source `/etc/honchol/env` first, then
  `deploy/honchol.env` (derived settings). Missing `CLOUDFLARE_ACCOUNT_ID` simply disables
  the judge — the server starts anyway and search degrades fail-open.

## Install (first time)

    sudo bash deploy/install.sh        # lays out /opt/honchol, builds, installs+enables units
    sudoedit /etc/honchol/env          # fill in OPENCODE_GO_API_KEY (+ CLOUDFLARE_* for the judge)
    systemctl start honchol-serve honchol-derive.timer honchol-backup.timer
    curl -s http://127.0.0.1:8792/health

The installer also seeds `/etc/honchol/env` as a mode-600 placeholder.

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

## Restore

    systemctl stop honchol-serve
    rm -f data/honchol.db-wal data/honchol.db-shm
    cp data/backups/honchol-<TS>.db data/honchol.db
    systemctl start honchol-serve
    # verify: ./honchol doctor -db data/honchol.db   +   curl /health

## Known limitations (import path)

- `cards` / `summaries` are not carried over from an export database — cards rebuild
  from new messages via derive.
- Conclusion provenance (`session_id` / `level` / sources) is backfilled with fixed
  values where the source export lacks it.
- Imported messages are marked `processed=1` (they won't be re-derived).
- Conclusion search is literal (FTS5/trigram) + judge re-ranking; there is no
  embedding/semantic tier yet.

## Security notes

- **No authentication.** The server binds to loopback by default; a non-loopback
  bind is refused unless `HONCHO_LITE_ALLOW_NON_LOOPBACK=1` is set explicitly.
  (An empty host like `:8792` counts as non-loopback.)
- Keep `/etc/honchol/env` readable only by root (the installer creates it as 0600).
- Backups live on the same disk (single-disk); no external alerting — watch the
  `/health` `degraded` flag or the journal.
