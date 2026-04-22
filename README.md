# AI Status

Local web dashboard for Claude Code (or any agent) to report progress while it works. The agent writes Markdown to a session file; this app watches the file and renders it live in your browser.

Pairs with the bundled **status-orchestrator** skill, which turns Claude into an orchestrator: it maintains the status file, delegates work to background subagents, and holds them to a quality bar.

![AI Status dashboard](docs/screenshot.png)

## What it does

- Runs a small Go server on `http://127.0.0.1:7879`.
- Manages a list of sessions (create, rename, pin, archive).
- Each session = one `.md` file on disk, under `./sessions/`.
- Click a session title in the sidebar to copy its absolute path — paste that into Claude so it knows where to write.
- Page re-renders live (SSE) every time the file changes.
- Optional desktop notifications when a session changes while the tab is hidden.
- System-tray icon with Open / Copy URL / Quit.

## Requirements

- Windows 10 / 11
- Go 1.22+ (only to build from source)

## Install

Download the prebuilt `ai-status.exe` from the [GitHub Releases](https://github.com/jwillmer/ai-status/releases) page, or build from source:

```
go build -ldflags="-H windowsgui" -o ai-status.exe
```

The `-H windowsgui` flag hides the console window. Omit it while developing if you want stdout.

## Run

```
ai-status.exe
```

Opens the browser automatically and adds a tray icon. Sessions and app data are written under the working directory (`./sessions/`, `./data/`).

### Flags

| Flag | Default | Purpose |
|---|---|---|
| `-addr` | `127.0.0.1:7879` | Listen address |
| `-root` | `.` | Data root (holds `sessions/`, `data/`, log) |
| `-no-tray` | `false` | Run without system-tray icon |
| `-no-open` | `false` | Don't auto-open the browser |

## Companion skill

The `status-orchestrator` skill is bundled at `skill/status-orchestrator/SKILL.md` and exposed as a `.skill` download from the app (and from GitHub).

Install in Claude: **Customize → Skills → Install skill**, select the downloaded `.skill` file.

Source: [`skill/status-orchestrator/SKILL.md`](skill/status-orchestrator/SKILL.md)

## Usage

1. Launch `ai-status.exe`. The dashboard opens at `http://127.0.0.1:7879`.
2. Click **New session** — a fresh `.md` file is created under `sessions/`.
3. Click the session title to copy its absolute path.
4. In Claude, paste the path and ask it to use it (the skill recognises the pattern automatically).
5. Work as normal. Claude writes status; the page updates live.

## Data layout

```
<root>/
├── sessions/          # one .md per session
├── data/
│   └── sessions.json  # session metadata (titles, pin, archive state)
└── status-updates.log # server log (only visible in windowsgui builds)
```

All files are plain text. Safe to back up, diff, or edit by hand.

## License

MIT
