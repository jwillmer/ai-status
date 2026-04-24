//go:build windows

package main

import (
	"context"

	"github.com/UserExistsError/conpty"
)

// ptyIO is the platform-neutral PTY handle consumed by TerminalManager.
// Windows uses ConPTY; Unix uses creack/pty (see pty_unix.go).
type ptyIO interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(cols, rows int) error
	Close() error
	Wait(ctx context.Context) (uint32, error)
}

type windowsPTY struct {
	c *conpty.ConPty
}

func (p *windowsPTY) Read(b []byte) (int, error)               { return p.c.Read(b) }
func (p *windowsPTY) Write(b []byte) (int, error)              { return p.c.Write(b) }
func (p *windowsPTY) Close() error                             { return p.c.Close() }
func (p *windowsPTY) Resize(cols, rows int) error              { return p.c.Resize(cols, rows) }
func (p *windowsPTY) Wait(ctx context.Context) (uint32, error) { return p.c.Wait(ctx) }

// startPTY launches cmd.exe with a ConPTY sized to cols×rows, cwd=folder.
func startPTY(folder string, cols, rows int) (ptyIO, error) {
	c, err := conpty.Start("cmd.exe",
		conpty.ConPtyWorkDir(folder),
		conpty.ConPtyDimensions(cols, rows))
	if err != nil {
		return nil, err
	}
	return &windowsPTY{c: c}, nil
}
