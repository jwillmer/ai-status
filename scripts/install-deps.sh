#!/usr/bin/env bash
# install-deps.sh — install everything ai-status needs to build and run
# on the current OS. Idempotent: anything already present is left alone.
#
# Linux  : Go, GTK + ayatana-appindicator (systray), pkg-config, zenity
#          (folder picker), xdg-utils (xdg-open). Distros: apt, dnf, pacman,
#          zypper. Picks whichever is on $PATH.
# macOS  : Go (via Homebrew). Folder picker + file/terminal launchers are
#          built in to the OS. Installs Homebrew if missing? — no, prints
#          a hint instead; we don't curl|bash anything without consent.
# Windows: not supported by this script. Prints the manual install hints.
#
# Usage:
#   ./scripts/install-deps.sh           # interactive, asks before sudo
#   ./scripts/install-deps.sh --yes     # non-interactive, assumes yes

set -euo pipefail

ASSUME_YES=0
for arg in "$@"; do
  case "$arg" in
    -y|--yes) ASSUME_YES=1 ;;
    -h|--help)
      sed -n '2,15p' "$0"
      exit 0
      ;;
    *)
      echo "unknown arg: $arg" >&2
      exit 2
      ;;
  esac
done

# Minimum Go version required by go.mod.
MIN_GO_MAJOR=1
MIN_GO_MINOR=22

c_red()   { printf '\033[0;31m%s\033[0m' "$*"; }
c_grn()   { printf '\033[0;32m%s\033[0m' "$*"; }
c_ylw()   { printf '\033[0;33m%s\033[0m' "$*"; }
c_dim()   { printf '\033[0;90m%s\033[0m' "$*"; }

log()  { printf '%s %s\n' "$(c_dim '[deps]')" "$*"; }
ok()   { printf '%s %s\n' "$(c_grn '  ok ')" "$*"; }
skip() { printf '%s %s\n' "$(c_dim 'skip ')" "$*"; }
warn() { printf '%s %s\n' "$(c_ylw 'warn ')" "$*"; }
fail() { printf '%s %s\n' "$(c_red 'fail ')" "$*" >&2; }

confirm() {
  # confirm "prompt" — returns 0 (yes) or 1 (no). Honours --yes.
  if [ "$ASSUME_YES" = "1" ]; then
    return 0
  fi
  printf '%s [y/N] ' "$1"
  read -r reply
  case "$reply" in
    y|Y|yes|YES) return 0 ;;
    *)           return 1 ;;
  esac
}

have() { command -v "$1" >/dev/null 2>&1; }

# Detect OS family.
detect_os() {
  case "$(uname -s)" in
    Linux)   echo linux ;;
    Darwin)  echo darwin ;;
    MINGW*|MSYS*|CYGWIN*) echo windows ;;
    *)       echo unknown ;;
  esac
}

# Pick a Linux package manager and define `pkg_install` accordingly.
detect_pm() {
  if have apt-get; then echo apt
  elif have dnf;    then echo dnf
  elif have pacman; then echo pacman
  elif have zypper; then echo zypper
  else echo unknown
  fi
}

# Linux package names per package manager. We index by a logical name and
# the per-PM table below resolves it to the real apt/dnf/pacman/zypper name.
pkg_name() {
  # $1 = pm, $2 = logical
  case "$2" in
    go)
      case "$1" in
        apt)    echo golang-go ;;
        dnf)    echo golang ;;
        pacman) echo go ;;
        zypper) echo go ;;
      esac
      ;;
    gtk3)
      case "$1" in
        apt)    echo libgtk-3-dev ;;
        dnf)    echo gtk3-devel ;;
        pacman) echo gtk3 ;;
        zypper) echo gtk3-devel ;;
      esac
      ;;
    appindicator)
      case "$1" in
        apt)    echo libayatana-appindicator3-dev ;;
        dnf)    echo libayatana-appindicator-gtk3-devel ;;
        pacman) echo libayatana-appindicator ;;
        zypper) echo libayatana-appindicator3-devel ;;
      esac
      ;;
    pkgconfig)
      case "$1" in
        apt)    echo pkg-config ;;
        dnf)    echo pkgconf-pkg-config ;;
        pacman) echo pkgconf ;;
        zypper) echo pkg-config ;;
      esac
      ;;
    zenity)   echo zenity ;;
    xdgutils)
      case "$1" in
        apt)    echo xdg-utils ;;
        dnf)    echo xdg-utils ;;
        pacman) echo xdg-utils ;;
        zypper) echo xdg-utils ;;
      esac
      ;;
  esac
}

pkg_install_cmd() {
  case "$1" in
    apt)    echo "sudo apt-get install -y" ;;
    dnf)    echo "sudo dnf install -y" ;;
    pacman) echo "sudo pacman -S --noconfirm --needed" ;;
    zypper) echo "sudo zypper install -y" ;;
  esac
}

pkg_update_cmd() {
  case "$1" in
    apt)    echo "sudo apt-get update" ;;
    *)      echo "" ;;  # others refresh as part of install
  esac
}

# Returns 0 if Go on PATH meets the minimum version, 1 otherwise.
go_ok() {
  have go || return 1
  v=$(go version 2>/dev/null | awk '{print $3}' | sed 's/^go//')
  major=${v%%.*}; rest=${v#*.}; minor=${rest%%.*}
  [ "$major" -gt "$MIN_GO_MAJOR" ] && return 0
  [ "$major" -eq "$MIN_GO_MAJOR" ] && [ "$minor" -ge "$MIN_GO_MINOR" ] && return 0
  return 1
}

# ---------- platform-specific install paths ----------

install_linux() {
  pm=$(detect_pm)
  if [ "$pm" = "unknown" ]; then
    fail "no supported package manager (apt/dnf/pacman/zypper) found"
    fail "install manually: Go >=${MIN_GO_MAJOR}.${MIN_GO_MINOR}, GTK3 + ayatana-appindicator dev headers, pkg-config, zenity, xdg-utils"
    return 1
  fi
  log "detected package manager: $pm"

  # 1) Build out the list of missing packages so we can prompt + install in
  #    a single sudo invocation. Each entry is "logical:realname".
  to_install=()

  if go_ok; then
    ok "go $(go version | awk '{print $3}') (>= ${MIN_GO_MAJOR}.${MIN_GO_MINOR})"
  else
    if have go; then
      warn "go found but older than required (${MIN_GO_MAJOR}.${MIN_GO_MINOR})"
    fi
    to_install+=("go:$(pkg_name "$pm" go)")
  fi

  # systray needs GTK3 + appindicator headers at build time.
  if pkg-config --exists gtk+-3.0 2>/dev/null; then
    ok "gtk+-3.0 dev headers"
  else
    to_install+=("gtk3:$(pkg_name "$pm" gtk3)")
  fi
  if pkg-config --exists ayatana-appindicator3-0.1 2>/dev/null \
     || pkg-config --exists appindicator3-0.1 2>/dev/null; then
    ok "ayatana-appindicator dev headers"
  else
    to_install+=("appindicator:$(pkg_name "$pm" appindicator)")
  fi
  if have pkg-config; then
    ok "pkg-config"
  else
    to_install+=("pkgconfig:$(pkg_name "$pm" pkgconfig)")
  fi

  # Folder picker — prefer zenity. kdialog/yad are also accepted by the app
  # but we don't bother installing them if they're missing.
  if have zenity || have kdialog || have yad; then
    ok "folder picker ($(have zenity && echo zenity || (have kdialog && echo kdialog || echo yad)))"
  else
    to_install+=("zenity:zenity")
  fi

  if have xdg-open; then
    ok "xdg-open"
  else
    to_install+=("xdgutils:$(pkg_name "$pm" xdgutils)")
  fi

  if [ "${#to_install[@]}" -eq 0 ]; then
    log "all dependencies already present"
    return 0
  fi

  log "missing packages:"
  for entry in "${to_install[@]}"; do
    printf '       - %s\n' "${entry##*:}"
  done

  if ! confirm "install with $pm via sudo?"; then
    warn "aborted — install the packages above manually, then re-run"
    return 1
  fi

  upd=$(pkg_update_cmd "$pm")
  if [ -n "$upd" ]; then
    log "$upd"
    eval "$upd"
  fi
  inst=$(pkg_install_cmd "$pm")
  pkgs=()
  for entry in "${to_install[@]}"; do
    pkgs+=("${entry##*:}")
  done
  log "$inst ${pkgs[*]}"
  eval "$inst ${pkgs[*]}"

  if go_ok; then
    ok "go installed: $(go version | awk '{print $3}')"
  else
    warn "go is still missing or too old after install — your distro repo may"
    warn "ship an old Go. Install from https://go.dev/dl/ and put it on PATH."
  fi
}

install_darwin() {
  if go_ok; then
    ok "go $(go version | awk '{print $3}') (>= ${MIN_GO_MAJOR}.${MIN_GO_MINOR})"
  else
    if have brew; then
      if confirm "install go via Homebrew?"; then
        brew install go
      else
        warn "skipped — install Go manually before building"
      fi
    else
      warn "Homebrew not found. Install Go from https://go.dev/dl/ or install"
      warn "Homebrew first (https://brew.sh) and re-run this script."
    fi
  fi
  ok "macOS provides osascript, open, and Terminal.app — no extra deps"
}

install_windows() {
  warn "this script does not install Windows dependencies."
  warn "install Go from https://go.dev/dl/ (the MSI puts it on PATH)."
  warn "everything else (PowerShell folder dialog, cmd /c start, conpty) is built in."
  return 0
}

# ---------- main ----------

os=$(detect_os)
log "OS: $os"
case "$os" in
  linux)   install_linux ;;
  darwin)  install_darwin ;;
  windows) install_windows ;;
  *)
    fail "unsupported OS: $(uname -s)"
    exit 1
    ;;
esac

log "done. Build with: go build -o ai-status ."
