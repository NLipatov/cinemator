package media

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

func TestRunCommandKeepsSuccessWhenContextIsCanceledAfterExit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	completed := filepath.Join(t.TempDir(), "completed")
	lateCancel := &cancelAfterCommandContext{Context: ctx, completed: completed, cancel: cancel}
	output, err := runCommand(lateCancel, nil, "sh", "-c", `printf output; touch "$1"`, "sh", completed)
	if err != nil || string(output) != "output" {
		t.Fatalf("successful command returned %q, %v", output, err)
	}
	if !errors.Is(lateCancel.Err(), context.Canceled) {
		t.Fatal("context was not canceled after command completion")
	}
}

type cancelAfterCommandContext struct {
	context.Context
	completed string
	cancel    context.CancelFunc
}

func (c *cancelAfterCommandContext) Err() error {
	// Cancel on the first error inspection after the command writes its marker.
	// This lets os/exec finish successfully before the late cancellation.
	if _, err := os.Stat(c.completed); err == nil {
		c.cancel()
	}
	return c.Context.Err()
}
