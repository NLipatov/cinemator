package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestSampleAnalyzerReturnsCanceledContextBeforeReading(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (SampleAnalyzer{}).Analyze(ctx, bytes.NewReader(nil))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Analyze() error = %v, want %v", err, context.Canceled)
	}
}
