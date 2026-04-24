//go:build !windows

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
)

// pickFolderNative pops a native folder chooser on Linux (zenity / kdialog)
// or macOS (osascript). Returns the selected absolute path, or "" on cancel.
// When no supported chooser is installed we return an error so the UI can
// surface a clear message instead of silently doing nothing.
func pickFolderNative() (string, error) {
	if runtime.GOOS == "darwin" {
		script := `POSIX path of (choose folder with prompt "Select working folder")`
		cmd := exec.Command("osascript", "-e", script)
		out, err := cmd.Output()
		if err != nil {
			// osascript exits non-zero when the user cancels; treat that as "".
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				return "", nil
			}
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	}

	// Linux/BSD — try zenity first (GTK), then kdialog (KDE), then yad.
	if _, err := exec.LookPath("zenity"); err == nil {
		cmd := exec.Command("zenity", "--file-selection", "--directory",
			"--title=Select working folder")
		out, err := cmd.Output()
		if err != nil {
			// zenity exits 1 when cancelled — swallow it.
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				return "", nil
			}
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := exec.LookPath("kdialog"); err == nil {
		home, _ := os.UserHomeDir()
		if home == "" {
			home = "/"
		}
		cmd := exec.Command("kdialog", "--getexistingdirectory", home,
			"--title", "Select working folder")
		out, err := cmd.Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				return "", nil
			}
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	}
	if _, err := exec.LookPath("yad"); err == nil {
		cmd := exec.Command("yad", "--file", "--directory",
			"--title=Select working folder")
		out, err := cmd.Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
				return "", nil
			}
			return "", err
		}
		return strings.TrimSpace(strings.TrimRight(string(out), "|")), nil
	}
	return "", fmt.Errorf("no folder picker available — install zenity, kdialog, or yad")
}

// openFileInDefaultApp opens path with the desktop's default handler.
// Linux uses xdg-open; macOS uses `open`.
func openFileInDefaultApp(path string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("open", path)
	} else {
		if _, err := exec.LookPath("xdg-open"); err != nil {
			return fmt.Errorf("xdg-open not found — install xdg-utils")
		}
		cmd = exec.Command("xdg-open", path)
	}
	// Detach so we can't zombie-hold the child if the server exits.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// linuxTerminal holds a terminal-emulator launch recipe.
//
// bin is the executable (e.g. "gnome-terminal"). Go's exec.LookPath resolves
// it against $PATH. args returns the full argv used to launch `innerCmd` in
// `folder`. innerCmd is a single bash -c string that runs `claude [args]`
// and then drops into an interactive shell (Windows `cmd /k` equivalent).
type linuxTerminal struct {
	bin  string
	args func(folder, innerCmd string) []string
}

var linuxTerminals = []linuxTerminal{
	{"gnome-terminal", func(folder, inner string) []string {
		return []string{"--working-directory=" + folder, "--", "bash", "-c", inner}
	}},
	{"konsole", func(folder, inner string) []string {
		return []string{"--workdir", folder, "-e", "bash", "-c", inner}
	}},
	{"xfce4-terminal", func(folder, inner string) []string {
		return []string{"--working-directory=" + folder, "--command", "bash -c " + shellQuote(inner)}
	}},
	{"mate-terminal", func(folder, inner string) []string {
		return []string{"--working-directory=" + folder, "--", "bash", "-c", inner}
	}},
	{"tilix", func(folder, inner string) []string {
		return []string{"--working-directory=" + folder, "-e", "bash", "-c", inner}
	}},
	{"alacritty", func(folder, inner string) []string {
		return []string{"--working-directory", folder, "-e", "bash", "-c", inner}
	}},
	{"kitty", func(folder, inner string) []string {
		return []string{"--directory", folder, "bash", "-c", inner}
	}},
	{"terminator", func(folder, inner string) []string {
		return []string{"--working-directory=" + folder, "-x", "bash", "-c", inner}
	}},
	// xterm doesn't grok --working-directory; cd inside the shell command.
	{"xterm", func(folder, inner string) []string {
		combined := "cd " + shellQuote(folder) + " && " + inner
		return []string{"-e", "bash", "-c", combined}
	}},
}

// openShellInFolder launches a detached terminal-emulator window in folder,
// running `claude [args]` then leaving an interactive shell open (matches the
// Windows `cmd /k` behaviour). On macOS we drive Terminal.app via osascript.
func openShellInFolder(folder string, claudeArgs []string) error {
	inner := "claude"
	for _, a := range claudeArgs {
		inner += " " + shellQuote(a)
	}
	// Hand control back to the user after claude exits (or dies) so they
	// can read output and type follow-up commands.
	inner += "; exec bash"

	if runtime.GOOS == "darwin" {
		script := fmt.Sprintf(
			`tell application "Terminal" to do script %q`,
			"cd "+shellQuote(folder)+" && "+inner,
		)
		cmd := exec.Command("osascript", "-e", script,
			"-e", `tell application "Terminal" to activate`)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		return cmd.Start()
	}

	// Linux: honour $TERMINAL if set and executable.
	if envTerm := strings.TrimSpace(os.Getenv("TERMINAL")); envTerm != "" {
		if _, err := exec.LookPath(envTerm); err == nil {
			// Best-effort: use the gnome-terminal-style argv.
			cmd := exec.Command(envTerm, "--working-directory="+folder, "--", "bash", "-c", inner)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := cmd.Start(); err == nil {
				return nil
			}
			// Fall through to the detected list if the custom invocation failed.
		}
	}

	for _, t := range linuxTerminals {
		if _, err := exec.LookPath(t.bin); err != nil {
			continue
		}
		cmd := exec.Command(t.bin, t.args(folder, inner)...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("no terminal emulator found — install gnome-terminal, konsole, xfce4-terminal, xterm, or set $TERMINAL")
}

// shellQuote wraps s in single quotes, escaping embedded single quotes per
// POSIX shell rules. Safe for paths and prompt strings passed to bash -c.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// trayIconBytes returns the bytes to hand to systray.SetIcon. Linux & macOS
// systray consumes PNG directly — no ICO wrapping required.
func trayIconBytes(sub fs.FS) []byte {
	data, err := fs.ReadFile(sub, "tray-icon.png")
	if err != nil {
		return nil
	}
	return data
}

// faviconBytes returns the bytes for /favicon.ico. We still wrap the PNG in
// an ICO container because browsers expect ICO at that path.
func faviconBytes(sub fs.FS) []byte {
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

// pathPlaceholder hints a native path shape for UI placeholders.
func pathPlaceholder() string {
	if runtime.GOOS == "darwin" {
		return "/Users/you/project"
	}
	return "/home/you/project"
}
