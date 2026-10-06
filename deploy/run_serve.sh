#!/usr/bin/env bash
# honchol serve launcher (used by systemd). Loads env files, then execs the server.
#
# Env files (both optional, read in this order):
#   /etc/honchol/env               — secrets: OPENCODE_GO_API_KEY, CLOUDFLARE_* ...
#   $INSTALL_DIR/deploy/honchol.env — derived settings (judge URL, overrides)
#
# Tolerance note: env files may contain lines a strict shell cannot parse
# (e.g. paths with spaces); -e is disabled while sourcing and only the
# essential variables are required.
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
  echo "warn: OPENCODE_GO_API_KEY not set — starting anyway (fail-open; chat/derive need it). Put it in $ENV_FILE" >&2
fi

cd "$INSTALL_DIR/server"
exec ./honchol serve -addr 127.0.0.1:8792 -db "$INSTALL_DIR/data/honchol.db"
