# Usage

1. Launch `ai-status` (or `ai-status.exe` on Windows). The dashboard opens at `http://127.0.0.1:7879`.
2. Click **New session** — a fresh `.md` file is created under `sessions/`.
3. Click the session title to copy its absolute path.
4. In Claude, paste the path and ask it to use it (the skill recognises the pattern automatically).
5. Work as normal. Claude writes status; the page updates live.

## Companion skill

The `status-orchestrator` skill turns Claude into an orchestrator: it maintains the status file, delegates work to background subagents, and holds them to a quality bar.

It's bundled at `skill/status-orchestrator/SKILL.md` and exposed as a `.skill` download from the app (and from GitHub). It's also embedded in the binary and loaded automatically for fresh sessions — see [features.md](features.md#skill-that-loads-itself) — so you usually don't need to install anything.

To install it manually in Claude: **Customize → Skills → Install skill**, then select the downloaded `.skill` file.

Source: [`skill/status-orchestrator/SKILL.md`](../skill/status-orchestrator/SKILL.md)

## Data layout

```
<root>/
├── sessions/          # one .md per session
├── data/
│   └── sessions.json  # session metadata (titles, pin, archive state)
└── status-updates.log # server log (only visible in windowsgui builds)
```

All files are plain text. Safe to back up, diff, or edit by hand. `<root>` defaults to the working directory; override with `-root` (see [install.md](install.md#flags)).
