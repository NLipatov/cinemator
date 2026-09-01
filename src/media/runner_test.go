package media

import (
	"context"
	"errors"
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
