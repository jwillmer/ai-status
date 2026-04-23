# ai-status — project instructions

## Rebuild / restart workflow (required after code changes)

Whenever you change **any source that ships in the binary** — `main.go`, anything under `static/` (HTML/CSS/JS), `skill/`, or the embedded icon — you must rebuild `ai-status.exe` and restart the running service so the user sees the change immediately. Do not leave it to the user.

Run this sequence after edits land (verified-working on this machine):

```bash
# 1. Stop the running instance — look up the PID explicitly, because
#    `taskkill //IM ai-status.exe` hits "Access is denied" against the GUI
#    build whereas `//PID <id>` works reliably.
PID=$(tasklist.exe //fi "imagename eq ai-status.exe" //fo csv 2>&1 \
      | grep ai-status | awk -F'"' '{print $4}')
[ -n "$PID" ] && taskkill.exe //PID "$PID" //F

# 2. Rebuild — MUST pass `-ldflags="-H windowsgui"` so the exe runs as a
#    Windows GUI app (no console window pops up when launched).
export PATH="/c/Program Files/Go/bin:$PATH"
go build -ldflags="-H windowsgui" -o ai-status.exe .

# 3. Launch detached WITHOUT opening a browser tab — the user already has one.
#    `&` is enough on this Windows+Bash setup; no need for nohup/disown.
./ai-status.exe --no-open &

# 4. Verify: the process is listed and the server answers on port 7879.
sleep 2 && tasklist.exe //fi "imagename eq ai-status.exe" //fo csv | grep ai-status
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:7879/   # expect 200
```

You can chain step 1–3 in a single Bash call (`&&` between the kill/build and a blank-line before the launch line) — the launch runs in the background so the Bash tool returns immediately.

Key rules:

- **Always pass `--no-open`.** The user already has a browser tab pointed at the dashboard; opening another is noise.
- **Always pass `-ldflags="-H windowsgui"`.** Without it, running the exe opens a visible cmd window behind the tray icon.
- **Never run `./ai-status.exe` in the foreground of the Bash tool** — it blocks the shell. Use `&` at the end, or `run_in_background: true` on the Bash call.
- **Kill by PID, not by image name.** `taskkill //IM ai-status.exe` returns "Access is denied"; the PID form works.
- **Don't use `cmd.exe //c start …` for detachment** — it also hits Access denied under the Bash tool. Plain `&` is sufficient.
- **Don't rebuild without killing first.** Go can't replace a running exe on Windows — the build will fail with "The process cannot access the file".

## Auto-reload in the browser

The UI auto-reloads after a server restart via the sticky connection banner: when the always-on `/api/events` SSE stream drops for ≥2.5s the banner shows; when it reconnects, `location.reload()` fires. So the correct restart dance is exactly the sequence above — the user's open tab will refresh itself within a few seconds of the new server being up.

If you add new client-side features that need fresh assets on reload, no extra work is needed — the reload picks them up.

## Build tooling

- Go binary lives at `C:\Program Files\Go\bin\go.exe` (not on `$PATH` in this shell).
- `go build -o ai-status.exe .` from the repo root. Windows icon resource comes from `rsrc_windows_amd64.syso` (prebuilt, don't touch).

## Do not use the PowerShell tool

Use the `Bash` tool for shell operations on this machine. The user has explicitly rejected the `PowerShell` tool.
