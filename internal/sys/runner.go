package sys

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

var (
	ErrJailDying    = errors.New("jail is in dying state or busy")
	ErrPoolCapacity = errors.New("zfs pool capacity exceeds safety threshold")
	ErrInvalidParam = errors.New("invalid or disallowed jail parameter")
)

// CmdResult holds captured stdout/stderr from a command execution.
type CmdResult struct {
	Stdout string
	Stderr string
}

// Runner abstracts OS command execution so FreeBSD primitives can be tested
// deterministically on non-FreeBSD development machines and CI.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (CmdResult, error)
	RunWithInput(ctx context.Context, stdin io.Reader, name string, args ...string) (CmdResult, error)
	RunStream(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error
}

// OSRunner executes real commands via os/exec.
type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, name string, args ...string) (CmdResult, error) {
	return OSRunner{}.RunWithInput(ctx, nil, name, args...)
}

func (OSRunner) RunWithInput(ctx context.Context, stdin io.Reader, name string, args ...string) (CmdResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := CmdResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return res, fmt.Errorf("%s %s: %s (%w)", name, strings.Join(args, " "), msg, err)
	}
	return res, nil
}

func (OSRunner) RunStream(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if stdin != nil {
		// Use StdinPipe instead of cmd.Stdin so cmd.Wait() returns as soon as
		// the child exits rather than deadlocking on an open interactive stream.
		pw, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			_ = pw.Close()
			return err
		}
		go func() {
			defer pw.Close()
			_, _ = io.Copy(pw, stdin)
		}()
		return cmd.Wait()
	}

	return cmd.Run()
}
