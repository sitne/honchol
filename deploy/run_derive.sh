#!/usr/bin/env bash
# honchol derive launcher (used by the systemd timer). Same env handling as run_serve.sh.
set -euo pipefail
INSTALL_DIR="${INSTALL_DIR:-/opt/honchol}"
ENV_FILE="${ENV_FILE:-/etc/honchol/env}"

set -a
set +e
[ -f "$ENV_FILE" ] && . "$ENV_FILE"
_rc1=$?
[ -f "$INSTALL_DIR/deploy/honchol.env" ] && . "$INSTALL_DIR/deploy/honchol.env"
_rc2=$?
set -e
set +a
[ "${_rc1:-0}" -eq 0 ] || echo "warn: $ENV_FILE load rc=$_rc1 (tolerated)" >&2
[ "${_rc2:-0}" -eq 0 ] || echo "warn: honchol.env load rc=$_rc2 (tolerated)" >&2

if [ -z "${OPENCODE_GO_API_KEY:-}" ]; then
  echo "error: OPENCODE_GO_API_KEY not set (put it in $ENV_FILE)" >&2
  exit 1
fi

cd "$INSTALL_DIR/server"
exec ./honchol derive -db "$INSTALL_DIR/data/honchol.db"
