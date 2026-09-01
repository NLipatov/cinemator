package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

func runCommand(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("run command: empty binary name")
	}

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()

	// Build suffix with captured output (if any).
	var parts []string
	if errBuf.Len() > 0 {
		parts = append(parts, "stderr:\n"+strings.TrimSpace(errBuf.String()))
	}
	if outBuf.Len() > 0 {
		parts = append(parts, "stdout:\n"+strings.TrimSpace(outBuf.String()))
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
