# Install & run

The short version is in the [README](../README.md#install--run). This page has the full details: requirements, building from source, runtime flags, and how the Windows app registration works.

## Requirements

- Windows 10 / 11, or Ubuntu 22.04+ / any modern Linux desktop, or macOS 12+
- Go 1.22+ (only to build from source)

## Get the binary

Download the prebuilt binary for your OS from the [GitHub Releases](https://github.com/jwillmer/ai-status/releases) page, or build from source (below).

### One-shot dependency install (Linux & macOS)

```
./scripts/install-deps.sh           # interactive: asks before sudo
./scripts/install-deps.sh --yes     # non-interactive
```

Detects `apt` / `dnf` / `pacman` / `zypper` (Linux) or `brew` (macOS), checks each dep with `pkg-config` / `command -v`, and installs only what's missing. Idempotent — safe to re-run.

### Build from source

**Windows:**

```
go build -ldflags="-H windowsgui" -o ai-status.exe
```

The `-H windowsgui` flag hides the console window. Omit it while developing if you want stdout.

**Linux / macOS:**

```
./scripts/build.sh           # kill running instance, build, relaunch detached, verify
./scripts/build.sh -b        # build only
./scripts/build.sh -n        # kill + build, don't relaunch
```

Or directly: `go build -o ai-status .`

## Run

**Windows:** `ai-status.exe`  
**Linux / macOS:** `./ai-status` (or `./scripts/build.sh` to rebuild and relaunch in one step)

Opens the browser automatically and adds a tray icon. Sessions and app data are written under the working directory (`./sessions/`, `./data/`).

### Flags

| Flag | Default | Purpose |
|---|---|---|
| `-addr` | `127.0.0.1:7879` | Listen address |
| `-root` | `.` | Data root (holds `sessions/`, `data/`, log) |
| `-no-tray` | `false` | Run without system-tray icon |
| `-no-open` | `false` | Don't auto-open the browser |
| `-no-register` | `false` | (Windows) Don't auto-register in the Start menu / Installed apps list |
| `-uninstall` | `false` | (Windows) Remove the Start menu / Installed apps registration, then exit |

## Finding it in the app list

### Windows

On first run the exe registers itself per-user (no admin, no separate installer):

- A **Start menu** shortcut — so "AI Status" shows up in Start-menu search / All apps.
- An entry in **Settings → Apps → Installed apps** — with the app icon and a working **Uninstall** button (which runs `ai-status.exe --uninstall`).

Registration only ever adds a shortcut and a per-user registry entry; it never moves or deletes the exe, so a portable copy stays portable. The shortcut tracks wherever the exe currently lives — move the exe and re-run it and the shortcut is rewritten to the new path. Pass `-no-register` to skip registration, or run `ai-status.exe --uninstall` to remove the entries by hand. Uninstalling also stops any running instance (so the server frees the port and the tray icon disappears); it never touches the exe or your sessions/data.

### Linux

Use `./scripts/install-desktop.sh` to write a `.desktop` entry (run it again to uninstall). It also builds the binary and installs the icon. The entry then shows in your launcher / app grid.

### macOS

Drag a `.app` bundle to the Dock.

## OS-specific notes

### Linux runtime extras

- **Terminal emulator** (any of): `gnome-terminal`, `konsole`, `xfce4-terminal`, `mate-terminal`, `tilix`, `alacritty`, `kitty`, `terminator`, `xterm`. The app auto-detects; set `$TERMINAL` to force one. Already installed on stock Ubuntu Desktop.
- **Folder picker**: `zenity` (GNOME), `kdialog` (KDE), or `yad`. `sudo apt install zenity` on Ubuntu.
- **Default-handler opener**: `xdg-utils` (`xdg-open`). Already installed on Ubuntu Desktop.

### Linux build extras

`libgtk-3-dev` and `libayatana-appindicator3-dev` are required to build the system-tray integration:

```
sudo apt install build-essential libgtk-3-dev libayatana-appindicator3-dev pkg-config
```
