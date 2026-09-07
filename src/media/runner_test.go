package media

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestRunCommandPreservesCancellationCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runCommand(ctx, nil, "true")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runCommand() error = %v, want wrapped %v", err, context.Canceled)
	}
}

func TestRunCommandPreservesOutputAndExitError(t *testing.T) {
	output, err := runCommand(context.Background(), nil, "sh", "-c", "printf output; printf diagnostic >&2; exit 7")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("runCommand() error = %v, want exit code 7", err)
	}
	if string(output) != "output" || !strings.Contains(err.Error(), "stderr:\ndiagnostic") || !strings.Contains(err.Error(), "stdout:\noutput") {
		t.Fatalf("runCommand() = %q, %v; want captured stdout and stderr", output, err)
	}
}
