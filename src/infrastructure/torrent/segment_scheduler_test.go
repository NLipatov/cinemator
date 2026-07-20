package torrent

import (
	"context"
	"errors"
	"testing"
)

func TestSegmentSchedulerBoundsJobs(t *testing.T) {
	scheduler := newSegmentScheduler(1, 1)
	release, err := scheduler.reserveJob()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.reserveJob(); !errors.Is(err, errStreamJobQueueFull) {
		t.Fatalf("second reservation = %v, want queue full", err)
	}
	release()
	release()
	if _, err := scheduler.reserveJob(); err != nil {
		t.Fatalf("reservation after release = %v", err)
	}
}

func TestSegmentSchedulerHonorsCanceledTranscode(t *testing.T) {
	scheduler := newSegmentScheduler(1, 1)
	scheduler.transcodes <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run := false
	err := scheduler.transcode(ctx, func() error {
		run = true
		return nil
	})
	if !errors.Is(err, context.Canceled) || run {
		t.Fatalf("transcode() = %v, run=%t", err, run)
	}
}
