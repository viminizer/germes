// Package env runs shell commands: on this machine now, in Docker later.
package env

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// Environment runs one shell command in a folder.
type Environment interface {
	Run(ctx context.Context, cmd, dir string, timeout time.Duration) (Result, error)
}

// Result is what a command printed and how it ended. A non-zero exit code is
// a normal result, not an error.
type Result struct {
	Output   string // stdout and stderr, in order
	ExitCode int
	TimedOut bool
}

// Local runs commands on this machine with bash.
type Local struct{}

// NewLocal creates the local environment.
func NewLocal() *Local { return &Local{} }

// Run runs cmd with `bash -c` in dir. A fresh shell every call: cd and export
// do not carry over. Timeout or ctx cancel kills the whole process group.
func (l *Local) Run(ctx context.Context, cmd, dir string, timeout time.Duration) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(ctx, "bash", "-c", cmd)
	c.Dir = dir
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 2 * time.Second // do not hang on children that keep the pipes open
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	res := Result{Output: out.String(), TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded)}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		res.ExitCode = exit.ExitCode()
	case res.TimedOut || ctx.Err() != nil:
		res.ExitCode = -1
	default:
		return res, fmt.Errorf("run command: %w", err)
	}
	return res, nil
}
