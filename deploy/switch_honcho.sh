#!/usr/bin/env bash
# Switch the Honcho baseUrl inside a client config JSON between this honchol
# (local) and a remote upstream server.
#
# usage: switch_honcho.sh lite|remote|status [--force]
#   lite   -> $LITE_URL    (default http://127.0.0.1:8792)
#   remote -> $REMOTE_URL  (no default; export it, e.g. http://192.0.2.10:8001)
#   status -> print current baseUrl
#   (alias: "honcho" == "remote", kept for convenience)
#
# Config file: $HONCHO_JSON (default ~/.config/honcho/honcho.json).
# Test with:   HONCHO_JSON=/tmp/x.json deploy/switch_honcho.sh lite
#
# - atomic rewrite (tmp + fsync + rename)
# - only the baseUrl value is replaced; everything else stays byte-identical
#   (single exact needle + full JSON validation after the swap)
# - "lite" runs a /health preflight first (abort on failure; --force overrides)
# - keeps 10 .bak-* generations
set -euo pipefail
F="${HONCHO_JSON:-$HOME/.config/honcho/honcho.json}"
LITE_URL="${LITE_URL:-http://127.0.0.1:8792}"
NEW=""
case "${1:-}" in
  lite)   NEW="$LITE_URL" ;;
  remote|honcho) NEW="${REMOTE_URL:?set REMOTE_URL (e.g. export REMOTE_URL=http://192.0.2.10:8001)}" ;;
  status)
    python3 - "$F" <<'EOF'
import json, sys
print("baseUrl =", json.load(open(sys.argv[1], encoding="utf-8"))["baseUrl"])
EOF
    exit 0 ;;
  *) echo "usage: $0 lite|remote|status  [--force]"; exit 1 ;;
esac

# preflight: only when switching to the local server
if [[ "$NEW" == "$LITE_URL" ]]; then
  if ! curl -sf -m 5 "$LITE_URL/health" >/dev/null; then
    echo "preflight FAILED: $LITE_URL/health unreachable" >&2
    if [[ "${2:-}" == "--force" ]]; then echo "  (--force: continuing)" >&2; else exit 1; fi
  fi
fi

# no-op check before touching anything (don't create a backup when nothing changes)
CUR=$(python3 - "$F" <<'EOF2'
import json, sys
try:
    print(json.load(open(sys.argv[1], encoding="utf-8"))["baseUrl"])
except Exception:
    print("")
EOF2
)
if [ "$CUR" = "$NEW" ]; then
  echo "already: $NEW (no change, no backup)"
  exit 0
fi

cp -a "$F" "$F.bak-$(date +%Y%m%d-%H%M%S)"

python3 - "$F" "$NEW" <<'EOF'
import json, os, sys, tempfile
f, new = sys.argv[1], sys.argv[2]
s = open(f, encoding="utf-8").read()
d = json.loads(s)
old = d["baseUrl"]
if old == new:
    print(" already:", new); sys.exit(0)

needle = None
for tmpl in ('"baseUrl": "%s"', '"baseUrl":"%s"'):
    c = s.count(tmpl % old)
    if c == 1:
        needle = tmpl % old; break
    if c > 1:
        raise SystemExit("needle appears %d times; refusing" % c)
if needle is None:
    raise SystemExit("exact baseUrl needle not found; refusing")

s2 = s.replace(needle, needle.replace(old, new))
d2 = json.loads(s2)
assert d2["baseUrl"] == new
# everything except baseUrl must stay identical (JSON-structure check)
assert {k: v for k, v in d2.items() if k != "baseUrl"} == {k: v for k, v in d.items() if k != "baseUrl"}

dirn = os.path.dirname(os.path.abspath(f))
fd, tmp = tempfile.mkstemp(dir=dirn, prefix=".honcho.json.tmp")
try:
    with os.fdopen(fd, "w", encoding="utf-8") as fp:
        fp.write(s2); fp.flush(); os.fsync(fp.fileno())
    os.replace(tmp, f)
finally:
    if os.path.exists(tmp):
        os.remove(tmp)
print(" switched:", old, "->", new)
EOF

# prune .bak-* to 10 generations
ls -1t "$F".bak-* 2>/dev/null | tail -n +11 | xargs -r rm --

# postcheck
python3 - "$F" <<'EOF'
import json, sys
print("verify: baseUrl =", json.load(open(sys.argv[1], encoding="utf-8"))["baseUrl"])
EOF
echo "note: takes effect for new client sessions. Rollback = run with the opposite side."
