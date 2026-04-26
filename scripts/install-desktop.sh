#!/usr/bin/env bash
# install-desktop.sh — toggle ai-status as a Linux desktop application.
#
# Run with no args: detects current state and prompts.
#   - if no entry installed → offers to install
#   - if entry already installed → offers to uninstall
#
# Files written on install:
#   ~/.local/share/applications/ai-status.desktop
#   ~/.local/share/icons/ai-status.png
#
# Non-interactive flags (skip the prompt):
#   --install    | -i    install (deps + build + entry)
#   --uninstall  | -u    remove the entry + icon
#   --no-deps            (install only) skip the deps check
#   --no-build           (install only) skip the build (use existing binary)
#   --yes        | -y    assume yes to all prompts
#   -h | --help          show this help

set -euo pipefail

ACTION=""           # "" = auto, "install", "uninstall"
SKIP_DEPS=0
SKIP_BUILD=0
ASSUME_YES=0
for arg in "$@"; do
  case "$arg" in
    -i|--install)        ACTION=install ;;
    -u|--uninstall|-r|--remove) ACTION=uninstall ;;
    --no-deps)           SKIP_DEPS=1 ;;
    --no-build)          SKIP_BUILD=1 ;;
    -y|--yes)            ASSUME_YES=1 ;;
    -h|--help)           sed -n '2,21p' "$0"; exit 0 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

if [ "$(uname -s)" != "Linux" ]; then
  echo "install-desktop.sh is Linux-only." >&2
  echo "On macOS use the Dock (drag a .app bundle); on Windows pin from the Start menu." >&2
  exit 1
fi

# Run from the repo root regardless of where the script is invoked from.
cd "$(dirname "$0")/.."
REPO_DIR=$(pwd)

DESKTOP_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/applications"
ICON_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/icons"
DESKTOP_FILE="$DESKTOP_DIR/ai-status.desktop"
ICON_FILE="$ICON_DIR/ai-status.png"

confirm() {
  # confirm "prompt" [default-yes]
  # returns 0 (yes) or 1 (no). Honours --yes.
  if [ "$ASSUME_YES" = "1" ]; then return 0; fi
  local prompt=$1
  local def=${2:-n}
  local hint
  if [ "$def" = "y" ]; then hint="[Y/n]"; else hint="[y/N]"; fi
  printf '%s %s ' "$prompt" "$hint"
  read -r reply
  if [ -z "$reply" ]; then reply=$def; fi
  case "$reply" in
    y|Y|yes|YES) return 0 ;;
    *)           return 1 ;;
  esac
}

# Auto-detect mode based on current state.
if [ -z "$ACTION" ]; then
  if [ -f "$DESKTOP_FILE" ]; then
    echo "AI Status desktop entry is currently installed:"
    echo "  $DESKTOP_FILE"
    if confirm "Uninstall it?" y; then
      ACTION=uninstall
    else
      echo "no changes made"
      exit 0
    fi
  else
    echo "AI Status desktop entry is not installed."
    echo "Installing will:"
    echo "  - check system dependencies (./scripts/install-deps.sh)"
    echo "  - build the binary if needed   (./scripts/build.sh --build-only)"
    echo "  - write $DESKTOP_FILE"
    echo "  - copy icon to $ICON_FILE"
    if confirm "Proceed with install?" y; then
      ACTION=install
    else
      echo "no changes made"
      exit 0
    fi
  fi
fi

# ---------- uninstall ----------

if [ "$ACTION" = "uninstall" ]; then
  removed=0
  if [ -f "$DESKTOP_FILE" ]; then
    rm -f "$DESKTOP_FILE"
    echo "[desktop] removed $DESKTOP_FILE"
    removed=1
  fi
  if [ -f "$ICON_FILE" ]; then
    rm -f "$ICON_FILE"
    echo "[desktop] removed $ICON_FILE"
    removed=1
  fi
  if command -v update-desktop-database >/dev/null 2>&1; then
    update-desktop-database "$DESKTOP_DIR" >/dev/null 2>&1 || true
  fi
  if [ "$removed" = "0" ]; then
    echo "[desktop] nothing to remove"
  else
    echo "[desktop] done — unpin manually from the dash if it was pinned"
  fi
  exit 0
fi

# ---------- install ----------

# 1) Ensure system deps (Go, GTK headers, zenity, …) are present.
if [ "$SKIP_DEPS" = "0" ]; then
  echo "[desktop] checking system dependencies"
  if [ "$ASSUME_YES" = "1" ]; then
    ./scripts/install-deps.sh --yes
  else
    ./scripts/install-deps.sh
  fi
fi

# 2) Ensure the binary is built and at the expected path.
BIN="$REPO_DIR/ai-status"
if [ "$SKIP_BUILD" = "0" ]; then
  echo "[desktop] building binary"
  ./scripts/build.sh --build-only
elif [ ! -x "$BIN" ]; then
  echo "[desktop] --no-build set but $BIN is missing — run ./scripts/build.sh first" >&2
  exit 1
fi

# 3) Install the icon. Prefer static/tray-icon.png (32x32 PNG, ships with
#    the binary). Skip gracefully if missing — entry just won't have an icon.
mkdir -p "$ICON_DIR"
if [ -f "$REPO_DIR/static/tray-icon.png" ]; then
  cp "$REPO_DIR/static/tray-icon.png" "$ICON_FILE"
  echo "[desktop] installed icon -> $ICON_FILE"
else
  echo "[desktop] warning: static/tray-icon.png not found; entry will be iconless" >&2
fi

# 4) Write the .desktop entry. Exec points at scripts/launch.sh — a wrapper
#    that opens a browser tab if the server is already up, otherwise starts
#    the binary. This makes the dash/taskbar click work even when GNOME
#    considers the app "running" (we have a tray icon but no top-level
#    window, so a "raise window" click would otherwise be a noop). No
#    StartupWMClass for the same reason — there's no window to associate.
LAUNCH="$REPO_DIR/scripts/launch.sh"
chmod +x "$LAUNCH" 2>/dev/null || true
mkdir -p "$DESKTOP_DIR"
cat > "$DESKTOP_FILE" <<EOF
[Desktop Entry]
Type=Application
Name=AI Status
GenericName=Claude Code Status Dashboard
Comment=Live dashboard for Claude Code session status files
Exec=$LAUNCH
Path=$REPO_DIR
Icon=ai-status
Terminal=false
Categories=Development;Utility;
Keywords=claude;ai;status;dashboard;
StartupNotify=true
EOF
chmod 0644 "$DESKTOP_FILE"
echo "[desktop] installed entry -> $DESKTOP_FILE"

# 5) Refresh the desktop DB and icon cache so launchers pick it up immediately.
if command -v update-desktop-database >/dev/null 2>&1; then
  update-desktop-database "$DESKTOP_DIR" >/dev/null 2>&1 || true
fi
if command -v gtk-update-icon-cache >/dev/null 2>&1; then
  gtk-update-icon-cache -q "$ICON_DIR" >/dev/null 2>&1 || true
fi

echo
echo "[desktop] done. Open Activities, search 'AI Status', launch it,"
echo "          then right-click its dash icon and 'Pin to Dash' to keep it on the task bar."
echo "          Re-run this script to uninstall."
