//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

// ptyIO is the platform-neutral PTY handle consumed by TerminalManager.
// Unix uses creack/pty; Windows uses ConPTY (see pty_windows.go).
type ptyIO interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(cols, rows int) error
	Close() error
	Wait(ctx context.Context) (uint32, error)
}

type unixPTY struct {
	f    *os.File
	cmd  *exec.Cmd
	done chan struct{}
	code int
}

func (p *unixPTY) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *unixPTY) Write(b []byte) (int, error) { return p.f.Write(b) }

func (p *unixPTY) Resize(cols, rows int) error {
	return pty.Setsize(p.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// Close tears down the PTY master; the kernel sends SIGHUP to the child
// so the shell exits. If the child ignored SIGHUP we follow up with SIGKILL
// on its process group (the pty-allocated session leader).
func (p *unixPTY) Close() error {
	_ = p.f.Close()
	if p.cmd.Process != nil {
		// Kill the whole pty session so any job-controlled descendants die too.
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	}
	return nil
}

func (p *unixPTY) Wait(ctx context.Context) (uint32, error) {
	select {
	case <-ctx.Done():
		if p.cmd.Process != nil {
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		}
		<-p.done
		return uint32(p.code), ctx.Err()
	case <-p.done:
		return uint32(p.code), nil
	}
}

// startPTY launches the user's login shell with a PTY sized to cols×rows,
// cwd=folder. `$SHELL` wins when set; otherwise we try bash and fall back
// to /bin/sh so we work on bare containers too.
func startPTY(folder string, cols, rows int) (ptyIO, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		if _, err := exec.LookPath("bash"); err == nil {
			shell = "bash"
		} else {
			shell = "/bin/sh"
		}
	}
	c := exec.Command(shell, "-l")
	c.Dir = folder
	// Ensure TERM is set so ncurses apps (claude, vim, …) work out of the box.
	env := os.Environ()
	if os.Getenv("TERM") == "" {
		env = append(env, "TERM=xterm-256color")
	}
	c.Env = env
	// New process group so we can signal descendants cleanly on Close/Wait.
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	f, err := pty.StartWithSize(c, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	u := &unixPTY{f: f, cmd: c, done: make(chan struct{})}
	go func() {
		err := c.Wait()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				u.code = exitErr.ExitCode()
			} else {
				u.code = -1
			}
		} else {
			u.code = 0
		}
		close(u.done)
	}()
	return u, nil
}
