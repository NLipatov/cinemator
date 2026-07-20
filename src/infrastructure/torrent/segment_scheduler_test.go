package torrent

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSegmentSchedulerBoundsJobs(t *testing.T) {
	scheduler := newSegmentScheduler(1, 1)
	release, err := scheduler.reserveJob(false, func() {})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.reserveJob(false, func() {}); !errors.Is(err, errStreamJobQueueFull) {
		t.Fatalf("second reservation = %v, want queue full", err)
	}
	release()
	release()
	if _, err := scheduler.reserveJob(false, func() {}); err != nil {
		t.Fatalf("reservation after release = %v", err)
	}
}

func TestSegmentSchedulerHonorsCanceledTranscode(t *testing.T) {
	scheduler := newSegmentScheduler(1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run := false
	err := scheduler.transcode(ctx, false, cancel, func() error {
		run = true
		return nil
	})
	if !errors.Is(err, context.Canceled) || run {
		t.Fatalf("transcode() = %v, run=%t", err, run)
	}
}

func TestSegmentSchedulerForegroundPreemptsBackgroundJob(t *testing.T) {
	scheduler := newSegmentScheduler(1, 1)
	backgroundCanceled := make(chan struct{})
	releaseBackground, err := scheduler.reserveJob(true, func() { close(backgroundCanceled) })
	if err != nil {
		t.Fatal(err)
	}
	defer releaseBackground()

	releaseForeground, err := scheduler.reserveJob(false, func() {})
	if err != nil {
		t.Fatalf("foreground reservation = %v", err)
	}
	defer releaseForeground()
	select {
	case <-backgroundCanceled:
	case <-time.After(time.Second):
		t.Fatal("background job was not preempted")
	}
}

func TestSegmentSchedulerForegroundPreemptsBackgroundTranscode(t *testing.T) {
	scheduler := newSegmentScheduler(2, 1)
	backgroundCtx, cancelBackground := context.WithCancel(context.Background())
	backgroundStarted := make(chan struct{})
	backgroundDone := make(chan error, 1)
	go func() {
		backgroundDone <- scheduler.transcode(backgroundCtx, true, cancelBackground, func() error {
			close(backgroundStarted)
			<-backgroundCtx.Done()
			return backgroundCtx.Err()
		})
	}()
	select {
	case <-backgroundStarted:
	case <-time.After(time.Second):
		t.Fatal("background transcode did not start")
	}

	foregroundRan := make(chan struct{})
	foregroundDone := make(chan error, 1)
	go func() {
		foregroundDone <- scheduler.transcode(context.Background(), false, func() {}, func() error {
			close(foregroundRan)
			return nil
		})
	}()
	select {
	case <-foregroundRan:
	case <-time.After(time.Second):
		t.Fatal("foreground transcode did not preempt background")
	}
	if err := <-foregroundDone; err != nil {
		t.Fatalf("foreground transcode = %v", err)
	}
	if err := <-backgroundDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("background transcode = %v, want canceled", err)
	}
}
