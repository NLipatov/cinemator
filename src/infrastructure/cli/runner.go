// Package cli provides a tiny helper to run external commands
// (ffmpeg/ffprobe/etc.) and return bounded stdout plus a rich error message.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const maxCapturedOutput = 1 << 20

type captureBuffer struct {
	bytes.Buffer
	truncated bool
}

type processGuardsKey struct{}

// WithProcessGuards makes owned child processes inherit cache-owner lock file
// descriptions. If the parent dies abruptly, startup remains fenced until its
// old children and descendants have also exited.
func WithProcessGuards(ctx context.Context, files ...*os.File) context.Context {
	guards := make([]*os.File, 0, len(files))
	for _, file := range files {
		if file != nil {
			guards = append(guards, file)
		}
	}
	if len(guards) == 0 {
		return ctx
	}
	return context.WithValue(ctx, processGuardsKey{}, guards)
}

func processGuards(ctx context.Context) []*os.File {
	guards, _ := ctx.Value(processGuardsKey{}).([]*os.File)
	return guards
}

func (b *captureBuffer) Write(p []byte) (int, error) {
	written := len(p)
	remaining := maxCapturedOutput - b.Len()
	if remaining > 0 {
		_, _ = b.Buffer.Write(p[:min(len(p), remaining)])
	}
	b.truncated = b.truncated || len(p) > remaining
	return written, nil
}

// RunWithStdin executes a command with provided stdin and captures up to 1 MiB
// from each output stream.
func RunWithStdin(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("cli.RunWithStdin: empty binary name")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s canceled: %w", name, err)
	}

	cmd := exec.Command(name, args...)
	configureOwnedProcess(ctx, cmd)
	cmd.Stdin = stdin

	var outBuf, errBuf captureBuffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := runOwnedProcess(ctx, cmd)
	if runErr == nil && (outBuf.truncated || errBuf.truncated) {
		runErr = errors.New("captured output exceeded 1 MiB")
	}

	// Build suffix with captured output (if any).
	var parts []string
	if errBuf.Len() > 0 {
		parts = append(parts, "stderr:\n"+strings.TrimSpace(errBuf.String())+truncationSuffix(errBuf.truncated))
	}
	if outBuf.Len() > 0 {
		parts = append(parts, "stdout:\n"+strings.TrimSpace(outBuf.String())+truncationSuffix(outBuf.truncated))
	}
	suffix := strings.Join(parts, "\n\n")

	// Success (still honor rare case of already-canceled ctx).
	if runErr == nil {
		if ctx.Err() != nil {
			if suffix == "" {
				return outBuf.Bytes(), fmt.Errorf("%s canceled: %w", name, ctx.Err())
			}
			return outBuf.Bytes(), fmt.Errorf("%s canceled: %w\n%s", name, ctx.Err(), suffix)
		}
		return outBuf.Bytes(), nil
	}

	// Context canceled/timeout takes precedence.
	if ctx.Err() != nil {
		if suffix == "" {
			return outBuf.Bytes(), fmt.Errorf("%s canceled: %w", name, ctx.Err())
		}
		return outBuf.Bytes(), fmt.Errorf("%s canceled: %w\n%s", name, ctx.Err(), suffix)
	}

	// Non-zero exit? include exit code if available.
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		if suffix == "" {
			return outBuf.Bytes(), fmt.Errorf("%s failed (exit code %d)", name, ee.ExitCode())
		}
		return outBuf.Bytes(), fmt.Errorf("%s failed (exit code %d)\n%s", name, ee.ExitCode(), suffix)
	}

	// Spawn/setup error (binary not found, permission, etc.).
	if suffix == "" {
		return outBuf.Bytes(), fmt.Errorf("%s failed: %v", name, runErr)
	}
	return outBuf.Bytes(), fmt.Errorf("%s failed: %v\n%s", name, runErr, suffix)
}

func runOwnedProcess(ctx context.Context, cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		// A successful leader may still have left descendants in our process
		// group. The job pin is released only after the whole owned group has
		// been fenced.
		_ = signalOwnedProcess(cmd, true)
		return err
	case <-ctx.Done():
		_ = signalOwnedProcess(cmd, false)
	}

	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case err := <-waited:
		// The parent can exit before a descendant. Kill the still-owned process
		// group before cache job pins and temporary files are released.
		_ = signalOwnedProcess(cmd, true)
		return err
	case <-timer.C:
		_ = signalOwnedProcess(cmd, true)
		return <-waited
	}
}

func truncationSuffix(truncated bool) string {
	if truncated {
		return "\n[output truncated]"
	}
	return ""
}
