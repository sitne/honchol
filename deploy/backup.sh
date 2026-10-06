#!/usr/bin/env bash
# Online backup of honchol.db with generation management + integrity check.
set -euo pipefail
INSTALL_DIR="${INSTALL_DIR:-/opt/honchol}"
DB="$INSTALL_DIR/data/honchol.db"
DIR="$INSTALL_DIR/data/backups"
KEEP=14
install -d -m 700 "$DIR"
TS=$(date +%Y%m%d-%H%M%S)
OUT="$DIR/honchol-$TS.db"
sqlite3 "$DB" ".backup '$OUT'"
chmod 600 "$OUT"
CHK=$(sqlite3 "$OUT" "PRAGMA integrity_check;" | head -1)
[[ "$CHK" == "ok" ]] || { echo "integrity_check FAILED: $CHK"; exit 1; }
ls -1t "$DIR"/honchol-*.db 2>/dev/null | tail -n +$((KEEP+1)) | xargs -r rm --
echo "backup ok: $OUT ($(du -h "$OUT" | cut -f1), keep=$KEEP)"
