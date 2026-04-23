# ai-status — project instructions

## Rebuild / restart workflow (required after code changes)

Whenever you change **any source that ships in the binary** — `main.go`, anything under `static/` (HTML/CSS/JS), `skill/`, or the embedded icon — you must rebuild `ai-status.exe` and restart the running service so the user sees the change immediately. Do not leave it to the user.

Run this sequence after edits land:

```bash
# 1. Stop the running instance (if any)
taskkill //F //IM ai-status.exe 2>/dev/null

# 2. Rebuild
'/c/Program Files/Go/bin/go.exe' build -o ai-status.exe .

# 3. Start detached WITHOUT opening a browser tab — the user already has one open
nohup ./ai-status.exe --no-open > /tmp/ai-status.log 2>&1 &
disown
```

Key rules:

- **Always pass `--no-open`.** The user already has a browser tab pointed at the dashboard; opening another is noise.
- **Never run `./ai-status.exe` in the foreground of the Bash tool** — it blocks the shell. Use `nohup … &` + `disown`, or `run_in_background: true` on the Bash call.
- **Verify after start:** `tasklist //FI "IMAGENAME eq ai-status.exe"` should show the process, and `curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:7879/` should return `200`.
- If `taskkill` reports "Access is denied", fall back to finding the PID with `tasklist` and killing by `//PID <id>`.
- `cmd.exe //c start …` often hits `Access is denied` under this Bash tool — prefer `nohup` for detachment.

## Auto-reload in the browser

The UI auto-reloads after a server restart via the sticky connection banner: when the always-on `/api/events` SSE stream drops for ≥2.5s the banner shows; when it reconnects, `location.reload()` fires. So the correct restart dance is exactly the sequence above — the user's open tab will refresh itself within a few seconds of the new server being up.

If you add new client-side features that need fresh assets on reload, no extra work is needed — the reload picks them up.

## Build tooling

- Go binary lives at `C:\Program Files\Go\bin\go.exe` (not on `$PATH` in this shell).
- `go build -o ai-status.exe .` from the repo root. Windows icon resource comes from `rsrc_windows_amd64.syso` (prebuilt, don't touch).

## Do not use the PowerShell tool

Use the `Bash` tool for shell operations on this machine. The user has explicitly rejected the `PowerShell` tool.
