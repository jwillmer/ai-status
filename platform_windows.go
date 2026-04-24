//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"io/fs"
	"os/exec"
	"strings"
	"syscall"
)

// pickFolderNative spawns a Windows FolderBrowserDialog via PowerShell and
// returns the selected absolute path, or "" on cancel.
func pickFolderNative() (string, error) {
	script := `Add-Type -AssemblyName System.Windows.Forms;` +
		`$f = New-Object System.Windows.Forms.FolderBrowserDialog;` +
		`$f.Description = 'Select working folder';` +
		`$f.ShowNewFolderButton = $true;` +
		`if ($f.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { Write-Output $f.SelectedPath }`
	cmd := exec.Command("powershell", "-NoProfile", "-STA", "-WindowStyle", "Hidden", "-Command", script)
	// Hide the PowerShell console window that would otherwise flash on screen
	// while the FolderBrowserDialog spools up.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// openFileInDefaultApp opens path with the system's default handler.
// Windows: `cmd /c start "" <path>`. Empty `""` is the window-title arg that
// `start` consumes, so the real path always lands as the file arg.
func openFileInDefaultApp(path string) error {
	cmd := exec.Command("cmd", "/c", "start", "", path)
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
