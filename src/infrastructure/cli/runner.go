// Package cli provides a tiny helper to run external commands
// (ffmpeg/ffprobe/etc.) and return bounded stdout plus a rich error message.
package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

const maxCapturedOutput = 1 << 20

type captureBuffer struct {
	bytes.Buffer
	truncated bool
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

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin

	var outBuf, errBuf captureBuffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
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

func truncationSuffix(truncated bool) string {
	if truncated {
		return "\n[output truncated]"
	}
	return ""
}
