package torrent

import (
	"os"
	"testing"
	"time"
)

func TestCleanupKeepsStreamRegisteredUntilWorkersAndPublicationStop(t *testing.T) {
	root := t.TempDir()
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	key := streamKey{InfoHash: "hash", Index: 0, Audio: 0, Subtitle: -1}
	paths := key.paths(root)
	if err := os.MkdirAll(paths.outDir, 0755); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	job := &segmentJob{done: make(chan struct{})}
	stream := &streamInfo{
		ready:        ready,
		source:       &torrentSource{},
		paths:        paths,
		cleanupDone:  make(chan struct{}),
		videoJobs:    map[*segmentJob]struct{}{job: {}},
		subtitleJobs: make(map[*segmentJob]struct{}),
	}
	manager := &manager{active: map[streamKey]*streamInfo{key: stream}, assets: assets}
	finished := make(chan struct{})
	go func() {
		manager.cleanup(key)
		close(finished)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		stream.mtx.Lock()
		closing := stream.closing
		stream.mtx.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup did not start")
		}
		time.Sleep(time.Millisecond)
	}
	manager.mu.Lock()
	registered := manager.active[key] == stream
	manager.mu.Unlock()
	if !registered {
		t.Fatal("closing stream was unregistered before its worker exited")
	}

	stream.generationMtx.Lock()
	close(job.done)
	select {
	case <-finished:
		stream.generationMtx.Unlock()
		t.Fatal("cleanup retired output during an active generation publication")
	case <-time.After(20 * time.Millisecond):
	}
	stream.generationMtx.Unlock()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish")
	}
	manager.mu.Lock()
	_, registered = manager.active[key]
	manager.mu.Unlock()
	if registered {
		t.Fatal("cleaned stream remains registered")
	}
}
