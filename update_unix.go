//go:build !windows

package main

import (
	"os"
	"syscall"
)

// swapAndRelaunch overwrites the running binary in place (the inode
// survives, so this works while we're still executing) and execs into
// it. Never returns on success.
func swapAndRelaunch(exePath, newPath string) error {
	if err := os.Chmod(newPath, 0755); err != nil {
		return err
	}
	if err := os.Rename(newPath, exePath); err != nil {
		return err
	}
	return syscall.Exec(exePath, os.Args, os.Environ())
}

func cleanupStaleBinary(_ string) {}
