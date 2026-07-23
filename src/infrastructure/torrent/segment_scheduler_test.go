package torrent

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSegmentSchedulerBoundsJobs(t *testing.T) {
	scheduler := newSegmentScheduler(1, 1, 1)
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

func TestSegmentSchedulerCapacityWaitWakesOnRelease(t *testing.T) {
	scheduler := newSegmentScheduler(1, 1, 1)
	release, err := scheduler.reserveJob(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	waited := make(chan error, 1)
	go func() {
		waited <- scheduler.waitForJobCapacity(ctx)
	}()

	select {
	case err := <-waited:
		t.Fatalf("capacity wait returned before release: %v", err)
	default:
	}
	release()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("capacity wait error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("capacity wait did not wake after release")
	}
}

func TestSegmentSchedulerHonorsCanceledTranscode(t *testing.T) {
	scheduler := newSegmentScheduler(1, 1, 1)
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
	scheduler := newSegmentScheduler(1, 1, 1)
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
	scheduler := newSegmentScheduler(2, 1, 1)
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

func TestSegmentSchedulerPackagerDoesNotWaitForEncoder(t *testing.T) {
	scheduler := newSegmentScheduler(2, 1, 1)
	encoderCtx, cancelEncoder := context.WithCancel(context.Background())
	defer cancelEncoder()
	encoderStarted := make(chan struct{})
	encoderDone := make(chan error, 1)
	go func() {
		encoderDone <- scheduler.transcode(encoderCtx, false, cancelEncoder, func() error {
			close(encoderStarted)
			<-encoderCtx.Done()
			return encoderCtx.Err()
		})
	}()
	select {
	case <-encoderStarted:
	case <-time.After(time.Second):
		t.Fatal("encoder did not start")
	}

	packagerRan := make(chan struct{})
	packagerDone := make(chan error, 1)
	go func() {
		packagerDone <- scheduler.packageMedia(context.Background(), false, func() {}, func() error {
			close(packagerRan)
			return nil
		})
	}()
	select {
	case <-packagerRan:
	case <-time.After(time.Second):
		t.Fatal("packager waited for the occupied encoder lane")
	}
	if err := <-packagerDone; err != nil {
		t.Fatalf("packager = %v", err)
	}
	cancelEncoder()
	if err := <-encoderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("encoder = %v, want canceled", err)
	}
}
