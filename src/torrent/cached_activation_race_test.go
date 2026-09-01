//go:build darwin || linux

package torrent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestActivateCachedStreamRetriesWhenDeletionStartsDuringReadinessCheck(t *testing.T) {
	root := t.TempDir()
	id := strings.Repeat("d", 40)
	key := streamKey{InfoHash: id, Index: 0, Audio: 0, Subtitle: -1}
	paths := key.paths(filepath.Join(root, "hls"))
	if err := resetStreamOutput(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.masterPlaylist, []byte("#EXTM3U\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(paths.readyMarker, 0600); err != nil {
		t.Fatal(err)
	}

	m := &Manager{
		active:    make(map[streamKey]*streamInfo),
		streamOps: make(map[streamKey]chan struct{}),
		deletions: make(map[string]chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := &doneObservedContext{
		Context:   ctx,
		requested: make(chan struct{}),
	}
	type result struct {
		activated bool
		err       error
	}
	resultReady := make(chan result, 1)
	go func() {
		activated, err := m.activateCachedStream(observed, key, paths)
		resultReady <- result{activated: activated, err: err}
	}()

	deadline := time.Now().Add(time.Second)
	for {
		m.mu.Lock()
		operationReserved := m.streamOps[key] != nil
		m.mu.Unlock()
		if operationReserved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("activateCachedStream() did not reserve its stream operation")
		}
		time.Sleep(time.Millisecond)
	}

	deletionDone, err := m.reserveDownloadDeletion(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.readyMarker, []byte(streamReadyVersion), 0600); err != nil {
		t.Fatal(err)
	}

	select {
	case <-observed.requested:
	case got := <-resultReady:
		t.Fatalf("activateCachedStream() published during deletion: %#v", got)
	case <-time.After(time.Second):
		t.Fatal("activateCachedStream() did not wait for the deletion")
	}
	m.mu.Lock()
	_, active := m.active[key]
	_, operationReserved := m.streamOps[key]
	m.mu.Unlock()
	if active || operationReserved {
		t.Fatalf("cached stream state during deletion: active=%v, operationReserved=%v", active, operationReserved)
	}

	if err := os.RemoveAll(paths.outDir); err != nil {
		t.Fatal(err)
	}
	m.finishDownloadDeletion(id, deletionDone)
	select {
	case got := <-resultReady:
		if got.err != nil || got.activated {
			t.Fatalf("activateCachedStream() = %v, %v; want false, nil", got.activated, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("activateCachedStream() did not retry after deletion finished")
	}
}
