//go:build windows

package main

import (
	"os"
	"os/exec"
	"time"
)

// swapAndRelaunch performs the Windows update dance: rename the running
// exe out of the way (allowed under Windows file locking), drop the new
// build in its place, spawn the child telling it to wait for the port,
// then exit so our handle releases. The .old file is removed on next
// startup via cleanupStaleBinary.
func swapAndRelaunch(exePath, newPath string) error {
	oldPath := exePath + ".old"
	_ = os.Remove(oldPath)

	if err := os.Rename(exePath, oldPath); err != nil {
		return err
	}
	if err := os.Rename(newPath, exePath); err != nil {
		_ = os.Rename(oldPath, exePath)
		return err
	}

	cmd := exec.Command(exePath, os.Args[1:]...)
	cmd.Env = append(os.Environ(), "AI_STATUS_PORT_WAIT=10")
	if err := cmd.Start(); err != nil {
		// Roll back so the user isn't stranded with a broken exe.
		_ = os.Remove(exePath)
		_ = os.Rename(oldPath, exePath)
		return err
	}
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}

	// Brief pause so the OS records the child before we exit and free
	// the port; the child's AI_STATUS_PORT_WAIT covers any remaining gap.
	time.Sleep(300 * time.Millisecond)
	os.Exit(0)
	return nil
}

// cleanupStaleBinary deletes any ai-status.exe.old left from a prior
// update. Best-effort: AV locks or permission errors are swallowed so
// startup is never blocked by a failed cleanup.
func cleanupStaleBinary(exePath string) {
	_ = os.Remove(exePath + ".old")
}
