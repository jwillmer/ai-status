//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// hideConsole keeps a console child (git, cmd, powershell, go) from flashing
// a console window. The server exe is built with `-H windowsgui`, so it has
// no console of its own — without this, Windows allocates a fresh visible
// console for every console-subsystem child.
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}

// openFileInDefaultApp opens path with the system's default handler.
// Windows: `cmd /c start "" <path>`. Empty `""` is the window-title arg that
// `start` consumes, so the real path always lands as the file arg.
func openFileInDefaultApp(path string) error {
	cmd := exec.Command("cmd", "/c", "start", "", path)
	hideConsole(cmd)
	return cmd.Start()
}

// openShellInFolder launches a detached terminal window in folder, running
// `claude` with the given extra args (the shell stays open afterwards).
//
// Uses `cmd /c start "" /D <folder> cmd.exe /k claude ...` so the child is
// fully detached from the (GUI-subsystem) server process. Each claude arg is
// passed as its own Go exec arg — Go's EscapeArg handles spaces cleanly.
func openShellInFolder(folder string, claudeArgs []string) error {
	args := []string{"/c", "start", "", "/D", folder, "cmd.exe", "/k", "claude"}
	args = append(args, claudeArgs...)
	cmd := exec.Command("cmd", args...)
	return cmd.Start()
}

// trayIconBytes returns the bytes the systray library should consume for the
// tray icon. Windows systray expects an ICO blob.
func trayIconBytes(sub fs.FS) []byte {
	return wrapPNGAsICO(sub)
}

// faviconBytes returns an ICO payload for /favicon.ico. Same format on every
// OS — browsers consume it the same way.
func faviconBytes(sub fs.FS) []byte {
	return wrapPNGAsICO(sub)
}

// wrapPNGAsICO returns an ICO wrapping the embedded 32x32 tray-icon.png.
func wrapPNGAsICO(sub fs.FS) []byte {
	pngData, err := fs.ReadFile(sub, "tray-icon.png")
	if err != nil || len(pngData) == 0 {
		return nil
	}
	var ico bytes.Buffer
	binary.Write(&ico, binary.LittleEndian, uint16(0))            // reserved
	binary.Write(&ico, binary.LittleEndian, uint16(1))            // type=icon
	binary.Write(&ico, binary.LittleEndian, uint16(1))            // count
	ico.WriteByte(32)                                             // width
	ico.WriteByte(32)                                             // height
	ico.WriteByte(0)                                              // no palette
	ico.WriteByte(0)                                              // reserved
	binary.Write(&ico, binary.LittleEndian, uint16(1))            // planes
	binary.Write(&ico, binary.LittleEndian, uint16(32))           // bpp
	binary.Write(&ico, binary.LittleEndian, uint32(len(pngData))) // size
	binary.Write(&ico, binary.LittleEndian, uint32(22))           // offset
	ico.Write(pngData)
	return ico.Bytes()
}

// pathPlaceholder is a cosmetic hint surfaced to the UI via /api/config.
func pathPlaceholder() string {
	return `C:\path\to\project`
}

// ----- Start menu / Installed apps registration -----

const (
	appDisplayName = "AI Status"
	// Per-user uninstall key — no admin rights needed. Anything under this
	// hive shows up in Settings > Apps > Installed apps.
	uninstallSubkey = `Software\Microsoft\Windows\CurrentVersion\Uninstall\AI Status`
)

// startMenuShortcut returns the per-user Start-menu .lnk path. Putting the
// shortcut here is what makes the app turn up in Start-menu search / All apps.
func startMenuShortcut() (string, error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return "", fmt.Errorf("APPDATA not set")
	}
	return filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", appDisplayName+".lnk"), nil
}

// registerApp makes the running exe discoverable as an installed app:
// a Start-menu shortcut plus a per-user uninstall registry entry. Both
// pieces are idempotent so this is cheap to call on every startup — the
// shortcut is only rewritten when it doesn't already point at this exe,
// and the registry values are refreshed so DisplayVersion tracks
// self-updates.
func registerApp(exe, dir string) error {
	if err := ensureStartMenuShortcut(exe, dir); err != nil {
		return err
	}
	return ensureUninstallEntry(exe, dir)
}

func ensureStartMenuShortcut(exe, dir string) error {
	lnk, err := startMenuShortcut()
	if err != nil {
		return err
	}
	// Fast path: if a shortcut already points at *this* exe, leave it be — so
	// we don't spawn PowerShell on every boot. If the exe has since moved, the
	// old path won't be found in the .lnk and we fall through to rewrite it.
	if shortcutPointsTo(lnk, exe) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(lnk), 0o755); err != nil {
		return err
	}
	// Create the .lnk via the WScript.Shell COM object. Guarded by the
	// shortcutPointsTo check above, this PowerShell spawn happens only when
	// the shortcut is missing or stale (usually just first run), which beats
	// hand-rolling the binary shell-link format. Single-quoted PS strings
	// keep Windows backslash paths literal.
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	ps := fmt.Sprintf(
		`$s=(New-Object -ComObject WScript.Shell).CreateShortcut(%s);`+
			`$s.TargetPath=%s;$s.WorkingDirectory=%s;$s.IconLocation=%s;`+
			`$s.Description='Live dashboard for Claude Code session status';$s.Save()`,
		q(lnk), q(exe), q(dir), q(exe+",0"))
	// -WindowStyle Hidden alone isn't enough: the console host window appears
	// before powershell gets to parse the flag. hideConsole suppresses it at
	// CreateProcess time.
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", ps)
	hideConsole(cmd)
	return cmd.Run()
}

// shortcutPointsTo reports whether the .lnk at lnk targets exe. WScript.Shell
// embeds the absolute target path in the shortcut (ANSI in the LinkInfo block,
// UTF-16LE in the string-data section), so a cheap byte scan for the path in
// either encoding tells us if it's current — no COM read / PowerShell spawn on
// the happy path. A moved exe simply won't match, triggering a rewrite.
func shortcutPointsTo(lnk, exe string) bool {
	data, err := os.ReadFile(lnk)
	if err != nil {
		return false // missing/unreadable → (re)create it
	}
	if bytes.Contains(data, []byte(exe)) {
		return true
	}
	u := utf16.Encode([]rune(exe))
	b := make([]byte, len(u)*2)
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[i*2:], c)
	}
	return bytes.Contains(data, b)
}

func ensureUninstallEntry(exe, dir string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, uninstallSubkey, registry.WRITE)
	if err != nil {
		return err
	}
	defer k.Close()

	str := map[string]string{
		"DisplayName":     appDisplayName,
		"DisplayIcon":     exe,
		"Publisher":       "Jens Willmer",
		"InstallLocation": dir,
		// Quoted exe path so spaces survive; the GUI build exits silently.
		"UninstallString": `"` + exe + `" --uninstall`,
		"URLInfoAbout":    "https://github.com/jwillmer/ai-status",
	}
	if v := strings.TrimPrefix(Version, "v"); v != "" && v != "dev" {
		str["DisplayVersion"] = v
	}
	for name, val := range str {
		if err := k.SetStringValue(name, val); err != nil {
			return err
		}
	}
	// Hide the Modify/Repair buttons — there's nothing to modify or repair.
	_ = k.SetDWordValue("NoModify", 1)
	_ = k.SetDWordValue("NoRepair", 1)
	return nil
}

// stopRunningInstances terminates any already-running copy of *this* exe so
// `--uninstall` (e.g. the Settings uninstall button) doesn't leave a server
// holding the port and a tray icon behind. It matches on the full image path
// — not just the "ai-status.exe" name — and skips its own PID, so a same-named
// binary in another folder is left alone. TerminateProcess is a hard kill, so
// it bypasses the signal handler's termManager.KillAll() and may orphan any
// embedded terminal/PTY children; that's acceptable here since the user is
// uninstalling anyway.
func stopRunningInstances(exe string) error {
	self := uint32(os.Getpid())
	base := strings.ToLower(filepath.Base(exe))

	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))

	var firstErr error
	for err := windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if pe.ProcessID == self {
			continue
		}
		if strings.ToLower(windows.UTF16ToString(pe.ExeFile[:])) != base {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pe.ProcessID)
		if err != nil {
			continue
		}
		buf := make([]uint16, 1024)
		n := uint32(len(buf))
		if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err == nil &&
			strings.EqualFold(windows.UTF16ToString(buf[:n]), exe) {
			if err := windows.TerminateProcess(h, 0); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		windows.CloseHandle(h)
	}
	return firstErr
}

// unregisterApp reverses registerApp. It removes only the OS registration —
// never the exe or user data — so uninstalling a portable copy is safe.
func unregisterApp() error {
	var firstErr error
	if lnk, err := startMenuShortcut(); err == nil {
		if err := os.Remove(lnk); err != nil && !os.IsNotExist(err) {
			firstErr = err
		}
	} else {
		firstErr = err
	}
	if err := registry.DeleteKey(registry.CURRENT_USER, uninstallSubkey); err != nil && err != registry.ErrNotExist {
		if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
