#!/usr/bin/env bash
# Install honchol: lay out /opt/honchol, build the binary, install systemd units.
#
# Usage (from a repo checkout):  sudo bash deploy/install.sh
# Override the target with:      sudo INSTALL_DIR=/srv/honchol bash deploy/install.sh
set -euo pipefail
SRC="$(cd "$(dirname "$0")/.." && pwd)"
INSTALL_DIR="${INSTALL_DIR:-/opt/honchol}"

echo "== honchol install: $SRC -> $INSTALL_DIR"
[ "$(id -u)" = "0" ] || { echo "error: run as root" >&2; exit 1; }

# 1) prerequisites
if ! command -v go >/dev/null; then
  echo "error: Go toolchain not found (needed to build; see https://go.dev/dl/)" >&2
  exit 1
fi
if ! command -v sqlite3 >/dev/null; then
  echo "warn: sqlite3 CLI not found — the daily backup timer needs it (e.g. apt install sqlite3)" >&2
fi

# 2) lay out the install dir
if [ "$SRC" != "$INSTALL_DIR" ]; then
  mkdir -p "$INSTALL_DIR"
  cp -a "$SRC/server" "$SRC/deploy" "$SRC/sidecar" "$INSTALL_DIR/"
fi
mkdir -p "$INSTALL_DIR/data"
chmod 700 "$INSTALL_DIR/data"

# 3) build the binary
( cd "$INSTALL_DIR/server" && go build -o honchol . )

# 3b) python venv for the model sidecar (embedding arm; best effort)
if command -v python3 >/dev/null; then
  if [ ! -x "$INSTALL_DIR/venv/bin/python" ]; then
    python3 -m venv "$INSTALL_DIR/venv" \
      || echo "warn: venv creation failed — the sidecar (embedding arm) won't start" >&2
  fi
  if [ -x "$INSTALL_DIR/venv/bin/pip" ]; then
    "$INSTALL_DIR/venv/bin/pip" install --quiet --disable-pip-version-check \
      -r "$INSTALL_DIR/sidecar/requirements.txt" \
      || echo "warn: pip install failed — install sidecar/requirements.txt manually" >&2
  fi
else
  echo "warn: python3 not found — the sidecar (embedding arm) won't start" >&2
fi

# 4) secrets env file (edit after install)
if [ ! -f /etc/honchol/env ]; then
  mkdir -p /etc/honchol
  umask 077
  cat > /etc/honchol/env <<'EOF'
# honchol secrets — fill these in:
# OPENCODE_GO_API_KEY=...
# CLOUDFLARE_API_TOKEN=...
# CLOUDFLARE_ACCOUNT_ID=...
EOF
  chmod 600 /etc/honchol/env
  echo "wrote /etc/honchol/env (placeholder, mode 600) — edit it before starting"
fi

# 5) units
D="$INSTALL_DIR/deploy"
chmod +x "$D"/run_serve.sh "$D"/run_derive.sh "$D"/backup.sh "$D"/switch_honcho.sh
install -m 644 "$D/honchol-serve.service" /etc/systemd/system/
install -m 644 "$D/honchol-sidecar.service" /etc/systemd/system/
install -m 644 "$D/honchol-derive.service" /etc/systemd/system/
install -m 644 "$D/honchol-derive.timer" /etc/systemd/system/
install -m 644 "$D/honchol-backup.service" /etc/systemd/system/
install -m 644 "$D/honchol-backup.timer" /etc/systemd/system/
install -m 644 "$D/honchol-judge.service" /etc/systemd/system/   # optional; not enabled by default
systemctl daemon-reload
systemctl enable honchol-serve.service honchol-sidecar.service honchol-derive.timer honchol-backup.timer

echo
echo "installed. next steps:"
echo "  1) edit /etc/honchol/env (OPENCODE_GO_API_KEY; CLOUDFLARE_* enables the judge)"
echo "  2) optional: fetch the translation model:  python3 $INSTALL_DIR/sidecar/download_lfm2.py"
echo "     (bekko embedding downloads automatically on first sidecar start)"
echo "  3) systemctl start honchol-serve honchol-sidecar honchol-derive.timer honchol-backup.timer"
echo "  4) curl -s http://127.0.0.1:8792/health  and  systemctl list-timers | grep honchol"
