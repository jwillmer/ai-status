# ai-status — project instructions

## Full workflow for a new feature or fix

Every change follows the same five phases. Don't skip 3 or 5 — they're the difference between "done on disk" and "done for the user."

1. **Understand + edit.** Read the files you need (never guess the current state). Make the code/markup change. Keep the diff focused — don't refactor surrounding code unless the task asks for it.
2. **Update the status file** (`sessions/<current>.md`). If there's an active orchestration session, move the task through its phases: add it to *Active tasks* when starting, move to *Done (awaiting confirmation)* with a one-line result when the code is landed, append a timestamped entry to the *Agent log*. Update `focus:` in the YAML front matter so the dashboard header reflects what's being worked on right now. Use `Edit` for partial updates; never rewrite the whole file.
3. **Rebuild + restart** (if the change ships in the binary — see below). Kill the running exe, rebuild with the GUI flag, launch detached with `--no-open`. The exact commands are in the next section.
4. **Verify.** Confirm the new process is up (`tasklist` + `curl` → 200). For UI changes, confirm via the user's open tab (it auto-reloads); for backend changes, hit the relevant endpoint with `curl` when it's easy to do so.
5. **Wait to commit until the user asks.** Do NOT run `git commit` at natural stopping points, after rebuilds, or after landing a feature — wait for the user's explicit instruction. When they ask, stage only the files you edited (`git add <paths>` — not `-A`) and use a HEREDOC commit message per the Claude Code system-prompt conventions. Don't commit generated or scratch files (`data/`, `sessions/`, `tools/<scratch>/`).

### What triggers a rebuild?

| Edited file                             | Needs rebuild? |
|-----------------------------------------|----------------|
| `main.go`, `terminal.go`, `diff.go`     | **Yes**        |
| Anything under `static/` (HTML/CSS/JS)  | **Yes** (embedded via `//go:embed`) |
| Anything under `skill/`                 | **Yes** (SKILL.md is embedded, served to Claude at first-message time) |
| `go.mod` / `go.sum` / icon resource     | **Yes**        |
| `CLAUDE.md`, `README.md`, other docs    | No             |
| Files in `sessions/`, `data/`           | No (runtime data) |

When in doubt: rebuild. It's cheap (~2s) and the user's tab auto-reloads.

### Committing

- **Never auto-commit.** Only run `git commit` when the user explicitly asks ("commit", "commit this", "commit the changes", etc.). Not after rebuilds, not at "natural stopping points", not after a feature "feels done".
- When asked: stage specific file paths (not `-A`), one `feat:`/`fix:` commit per user-visible change is ideal, HEREDOC message focused on *why*.
- The user has already seen the diff via the live dashboard; keep the message focused on *why* the change was made, not *what* (the diff already says what).

## Rebuild / restart workflow (required after code changes)

Whenever you change **any source that ships in the binary** — `main.go`, anything under `static/` (HTML/CSS/JS), `skill/`, or the embedded icon — you must rebuild the binary and restart the running service so the user sees the change immediately. Do not leave it to the user.

### Windows

Run this sequence after edits land (verified-working on this machine):

```bash
# 1. Stop the running instance — look up the PID explicitly, because
#    `taskkill //IM ai-status.exe` hits "Access is denied" against the GUI
#    build whereas `//PID <id>` works reliably.
PID=$(tasklist.exe //fi "imagename eq ai-status.exe" //fo csv 2>&1 \
      | grep ai-status | awk -F'"' '{print $4}')
[ -n "$PID" ] && taskkill.exe //PID "$PID" //F

# 2. Rebuild — MUST pass `-ldflags="-H windowsgui"` so the exe runs as a
#    Windows GUI app (no console window pops up when launched). Also
#    stamp Version + CommitSHA so the in-app update check has something
#    to compare against origin/main on GitHub.
export PATH="/c/Program Files/Go/bin:$PATH"
SHA=$(git rev-parse HEAD)
VER=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
go build -ldflags="-H windowsgui -X main.Version=$VER -X main.CommitSHA=$SHA" -o ai-status.exe .

# 3. Launch detached WITHOUT opening a browser tab — the user already has one.
#    `&` is enough on this Windows+Bash setup; no need for nohup/disown.
./ai-status.exe --no-open &

# 4. Verify: the process is listed and the server answers on port 7879.
sleep 2 && tasklist.exe //fi "imagename eq ai-status.exe" //fo csv | grep ai-status
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:7879/   # expect 200
```

You can chain step 1–3 in a single Bash call (`&&` between the kill/build and a blank-line before the launch line) — the launch runs in the background so the Bash tool returns immediately.

Windows key rules:

- **Always pass `--no-open`.** The user already has a browser tab pointed at the dashboard; opening another is noise.
- **Always pass `-ldflags="-H windowsgui"`.** Without it, running the exe opens a visible cmd window behind the tray icon.
- **Never run `./ai-status.exe` in the foreground of the Bash tool** — it blocks the shell. Use `&` at the end, or `run_in_background: true` on the Bash call.
- **Kill by PID, not by image name.** `taskkill //IM ai-status.exe` returns "Access is denied"; the PID form works.
- **Don't use `cmd.exe //c start …` for detachment** — it also hits Access denied under the Bash tool. Plain `&` is sufficient.
- **Don't rebuild without killing first.** Go can't replace a running exe on Windows — the build will fail with "The process cannot access the file".

### Linux / macOS

Use the wrapper script — it does the kill → build → relaunch → verify dance:

```bash
./scripts/build.sh           # full restart cycle
./scripts/build.sh -b        # build only
./scripts/build.sh -n        # kill + build, don't relaunch
```

If you need to set up a fresh box first: `./scripts/install-deps.sh` (detects apt/dnf/pacman/zypper or brew, installs only what's missing, idempotent).

The script is the equivalent of:

```bash
pkill -x ai-status || true
go build -o ai-status .
./ai-status --no-open >/tmp/ai-status.log 2>&1 &
disown
sleep 1 && pgrep -x ai-status
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:7879/   # expect 200
```

Linux/macOS key rules:

- **Always pass `--no-open`** — same reason as Windows.
- **Build needs GTK headers** for the tray icon: `sudo apt install libgtk-3-dev libayatana-appindicator3-dev pkg-config`. If you forget, `go build` fails at `pkg-config -- ayatana-appindicator3-0.1`.
- **Use `pkill -x ai-status`** — the `-x` is important, otherwise it matches every process whose cmdline contains the string.
- **Redirect logs** (`>/tmp/ai-status.log 2>&1`) when launching detached. Unlike the Windows GUI build, the Linux binary writes the startup log banner to stdout.
- **Don't rebuild without killing first.** Replacing a running binary on Linux is safe at the filesystem layer (inode survives), but the old version keeps serving requests until it exits.
- **Mind the data root when rebuilding on Linux.** `--root .` resolves against the cwd at launch time, so where you start the binary decides where `data/sessions.json` and the default `sessions/` folder live. The dev script (`scripts/build.sh`) launches from the repo, so a user whose original instance was started elsewhere (e.g. `~`) will appear to "lose" their sessions after a rebuild — they're untouched at the *previous* root. To preserve their store, relaunch with the same cwd: `(cd <orig-root> && /home/jwi/GitHub/ai-status/ai-status --no-open >/tmp/ai-status.log 2>&1 &)`. Check `/home/jwi/data/sessions.json` first to see if a non-repo root is in use. The in-app self-update path (`update_unix.go`) preserves argv/cwd, so this only bites during dev rebuilds.

## Auto-reload in the browser

The UI auto-reloads after a server restart via the sticky connection banner: when the always-on `/api/events` SSE stream drops for ≥2.5s the banner shows; when it reconnects, `location.reload()` fires. So the correct restart dance is exactly the sequence above — the user's open tab will refresh itself within a few seconds of the new server being up.

If you add new client-side features that need fresh assets on reload, no extra work is needed — the reload picks them up.

## Build tooling

- **Windows:** Go binary lives at `C:\Program Files\Go\bin\go.exe` (not on `$PATH` in this shell). `go build -ldflags="-H windowsgui" -o ai-status.exe .` from the repo root. Windows icon resource comes from `rsrc_windows_amd64.syso` (prebuilt, don't touch).
- **Linux / macOS:** `go build -o ai-status .` from the repo root. Platform code is split under build tags: `pty_windows.go` / `pty_unix.go` for the PTY backend, `platform_windows.go` / `platform_unix.go` for folder picker, file-open, terminal-emulator launch, and tray-icon wrapping, `update_windows.go` / `update_unix.go` for the self-update binary swap (Windows uses rename-old + spawn-new + exit; Unix overwrites in place + `syscall.Exec`). Add new OS-specific code to the matching pair; keep `main.go`, `terminal.go`, and `update.go` platform-neutral.

## Do not use the PowerShell tool

Use the `Bash` tool for shell operations on this machine. The user has explicitly rejected the `PowerShell` tool.
