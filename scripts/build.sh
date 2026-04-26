#!/usr/bin/env bash
# build.sh — kill any running ai-status, rebuild, relaunch detached.
# Mirrors the rebuild/restart dance documented in CLAUDE.md.
#
# Linux / macOS only. On Windows, follow the Windows section of CLAUDE.md
# (the GUI build needs `-ldflags="-H windowsgui"` and `tasklist`/`taskkill`).
#
# Flags:
#   -b, --build-only   build only, don't kill or relaunch
#   -n, --no-launch    kill + build, but don't relaunch
#   -h, --help         show this help

set -euo pipefail

BUILD_ONLY=0
NO_LAUNCH=0
for arg in "$@"; do
  case "$arg" in
    -b|--build-only) BUILD_ONLY=1 ;;
    -n|--no-launch)  NO_LAUNCH=1 ;;
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

case "$(uname -s)" in
  Linux|Darwin) ;;
  *)
    echo "build.sh supports Linux and macOS only — see CLAUDE.md for Windows" >&2
    exit 1
    ;;
esac

# Run from the repo root regardless of where the script is invoked from.
cd "$(dirname "$0")/.."

BIN=./ai-status
LOG=/tmp/ai-status.log
URL=http://127.0.0.1:7879/

if ! command -v go >/dev/null 2>&1; then
  echo "go not found on PATH — run scripts/install-deps.sh first" >&2
  exit 1
fi

# 1) Stop the running instance (skip in build-only mode).
if [ "$BUILD_ONLY" = "0" ]; then
  if pgrep -x ai-status >/dev/null 2>&1; then
    echo "[build] stopping running ai-status"
    pkill -x ai-status || true
    # Give it a moment to release the binary file (mostly a courtesy on Linux;
    # the inode survives anyway, but it keeps the process table tidy).
    sleep 0.3
  fi
fi

# 2) Rebuild — stamp Version + CommitSHA so the in-app update check can
#    compare HEAD against origin/main on GitHub. Falls back to "dev" /
#    empty SHA when not in a git checkout (rare).
SHA=""; VER="dev"
if command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
  SHA=$(git rev-parse HEAD 2>/dev/null || echo "")
  VER=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
fi
LDFLAGS="-X main.Version=$VER -X main.CommitSHA=$SHA"
echo "[build] go build (Version=$VER CommitSHA=${SHA:0:7})"
go build -ldflags="$LDFLAGS" -o "$BIN" .

if [ "$BUILD_ONLY" = "1" ] || [ "$NO_LAUNCH" = "1" ]; then
  echo "[build] done — binary at $BIN"
  exit 0
fi

# 3) Launch detached without opening a browser tab. The user already has one;
#    auto-reload picks up the new build via the SSE banner.
echo "[build] launching detached (logs: $LOG)"
"$BIN" --no-open >"$LOG" 2>&1 &
disown

# 4) Verify.
sleep 1
if pgrep -x ai-status >/dev/null 2>&1; then
  echo "[build] running, pid $(pgrep -x ai-status)"
else
  echo "[build] process not found after launch — check $LOG" >&2
  exit 1
fi

code=$(curl -s -o /dev/null -w '%{http_code}' "$URL" || echo "000")
if [ "$code" = "200" ]; then
  echo "[build] $URL -> 200 OK"
else
  echo "[build] $URL -> $code (expected 200) — check $LOG" >&2
  exit 1
fi
