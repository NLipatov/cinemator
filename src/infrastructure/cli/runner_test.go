package cli

import (
	"context"
	"errors"
	"strings"
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

func TestCaptureBufferBoundsMemory(t *testing.T) {
	var buffer captureBuffer
	input := strings.Repeat("x", maxCapturedOutput+1)

	written, err := buffer.Write([]byte(input))

	if err != nil || written != len(input) {
		t.Fatalf("Write() = %d, %v; want %d, nil", written, err, len(input))
	}
	if buffer.Len() != maxCapturedOutput || !buffer.truncated {
		t.Fatalf("capture = %d bytes, truncated=%t", buffer.Len(), buffer.truncated)
	}
}
