package cli

import (
	"context"
	"errors"
	"testing"
)

func TestRunWithStdinPreservesCancellationCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := RunWithStdin(ctx, nil, "true")

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunWithStdin() error = %v, want wrapped %v", err, context.Canceled)
	}
}
