package torrent

import (
	"context"
	"testing"
	"time"
)

func TestDownloadEventBroadcasterNotifiesSubscribers(t *testing.T) {
	broadcaster := newDownloadEventBroadcaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := broadcaster.subscribe(ctx)
	broadcaster.notify()

	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for download event")
	}
}

func TestDownloadEventBroadcasterCoalescesUnreadEvents(t *testing.T) {
	broadcaster := newDownloadEventBroadcaster()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	events := broadcaster.subscribe(ctx)
	broadcaster.notify()
	broadcaster.notify()

	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first download event")
	}

	select {
	case <-events:
		t.Fatal("got duplicate event from coalesced buffer")
	default:
	}
}
