#!/usr/bin/env bash
# launch.sh — entry point used by the .desktop file.
#
# Click-from-dash semantics: if the server is already running we just open a
# browser tab; otherwise we start the binary (which opens its own tab and
# stays resident in the system tray). Either way the script exits quickly so
# GNOME Shell doesn't treat the launcher as a long-lived process.

set -euo pipefail

URL="http://127.0.0.1:7879/"
BIN_DIR=$(cd "$(dirname "$0")/.." && pwd)
BIN="$BIN_DIR/ai-status"

# 1) Server already up → just open a browser tab.
if curl -fsS -o /dev/null -m 1 "$URL"; then
  if command -v xdg-open >/dev/null 2>&1; then
    xdg-open "$URL" >/dev/null 2>&1 &
  fi
  exit 0
fi

# 2) Otherwise launch the binary detached. main.go opens the browser tab on
#    its own once the server is up, so we don't double-open here.
if [ ! -x "$BIN" ]; then
  echo "ai-status binary not found at $BIN — run scripts/build.sh" >&2
  exit 1
fi
nohup "$BIN" >/tmp/ai-status.log 2>&1 &
disown
exit 0
