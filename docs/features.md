# Features

A deeper tour of what AI Status does. For install/run see [install.md](install.md); for day-to-day use see [usage.md](usage.md).

## Embedded terminals per session

Open a Claude (or any) console directly inside the session's column — no Alt-Tab, no lost context:

- ConPTY-backed on Windows, `creack/pty`-backed on Linux & macOS; full key passthrough (Esc, Ctrl+B, etc.) so Claude Code's shortcuts work.
- Terminal survives tab switches — the PTY lives on the server. `× Close` kills it explicitly; nothing else does.
- `▶ Show cmd` reattaches to a running PTY, or resumes via `claude --resume <uuid>` when the session has a stored Claude session ID, or starts fresh otherwise.
- `Open cmd` launches a detached console in the session's working folder — same resume/fresh logic. Windows uses `cmd.exe`; Linux probes for `gnome-terminal`, `konsole`, `xfce4-terminal`, `xterm`, etc. (or honours `$TERMINAL`); macOS drives Terminal.app via AppleScript.
- Ctrl+C copies selected text; Ctrl+V pastes. Ctrl+C with no selection forwards the SIGINT as usual.

## Live diff highlighting

Every time the file is re-saved, blocks whose plain text appeared since the last version get an amber highlight in the rendered markdown — including table rows. The mark persists until the *next* update, so you never miss what changed while you were away.

## Skill that loads itself

The `status-orchestrator` skill is embedded in the binary and written to `data/status-orchestrator.SKILL.md` on startup. When you `▶ Start cmd` for a fresh session, the terminal runs:

```
claude "Read and follow <path-to-SKILL.md>, then use this for status: <path-to-session.md>"
```

So Claude adopts the orchestrator role immediately — no skill install required. (If you already installed the `.skill` zip from the help dialog, that's fine too; they're the same content.)

## Themes

Dark is the default. A sidebar-footer button cycles **System → Light → Dark**; the choice is stored as a `theme` cookie (1 year) and applied pre-paint (no flash on reload).

## Mobile drawer

Below 768px the sidebar collapses to an off-canvas drawer — tap the hamburger top-left to open. Tap the overlay or press Escape to close; selecting a session also closes it.

## YAML frontmatter metadata

Sessions store their metadata (`title`, `project_folder`, `claude_session`, `created`, `focus`) as YAML front matter at the top of the `.md` file. It renders invisibly in the dashboard (via `goldmark-meta`) but stays machine-readable. The header shows `focus` as a sub-title; the **Metadata** button toggles a panel with all fields.

## Other conveniences

- **Native folder picker** — Windows uses the real `FolderBrowserDialog`; Linux uses `zenity` / `kdialog` / `yad` (first one found); macOS uses an AppleScript `choose folder` prompt. Beats the sandboxed `webkitdirectory` which only exposes the folder name.
- **Open file** — opens the raw `.md` in your system's default editor (`start` on Windows, `xdg-open` on Linux, `open` on macOS).
- **Renaming the title** rewrites the YAML `title:` field in place and keeps the sidebar / browser tab in sync.
- **Auto-reload** — when the server restarts, the connection banner appears briefly; once the SSE stream reconnects, the tab reloads itself so you pick up new assets automatically.
- **Relative timestamps** in coarse buckets ("just now", "a moment ago", "X minutes ago", "an hour ago", …) so nothing ticks every second.
