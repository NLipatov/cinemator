package torrent

import (
	"context"
	"errors"
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
	manager := &manager{active: map[streamKey]*streamInfo{key: stream}, media: &mediaCache{assets: assets}}
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

func TestShutdownStreamsStopsWaitingAtItsDeadline(t *testing.T) {
	root := t.TempDir()
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer assets.Close()

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
	manager := &manager{active: map[streamKey]*streamInfo{key: stream}, media: &mediaCache{assets: assets}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err = manager.shutdownStreams(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdownStreams() error = %v, want deadline exceeded", err)
	}
	stream.mtx.Lock()
	closing := stream.closing
	stream.mtx.Unlock()
	if !closing {
		t.Fatal("shutdown did not cancel the active stream before waiting")
	}

	close(job.done)
	select {
	case <-stream.cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("stream cleanup did not finish after its worker stopped")
	}
}

func TestReuseOrRetireStreamNeverReturnsFailedSession(t *testing.T) {
	root := t.TempDir()
	assets, err := newHlsAssetStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer assets.Close()

	key := streamKey{InfoHash: "hash", Index: 0, Audio: 0, Subtitle: -1}
	paths := key.paths(root)
	if err := os.MkdirAll(paths.outDir, 0755); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	close(ready)
	stream := &streamInfo{
		ready:        ready,
		fatalErr:     errors.New("failed presentation"),
		source:       &torrentSource{},
		paths:        paths,
		cleanupDone:  make(chan struct{}),
		videoJobs:    make(map[*segmentJob]struct{}),
		subtitleJobs: make(map[*segmentJob]struct{}),
	}
	manager := &manager{
		active:      map[streamKey]*streamInfo{key: stream},
		media:       &mediaCache{assets: assets},
		torrentUses: make(map[string]*torrentUse),
	}

	manager.mu.Lock()
	got, cleanupDone := manager.reuseOrRetireStreamLocked(key, time.Now())
	manager.mu.Unlock()
	if got != nil || cleanupDone == nil {
		t.Fatalf("reuseOrRetireStreamLocked() = (%p, %v), want cleanup only", got, cleanupDone)
	}
	stream.mtx.Lock()
	closing := stream.closing
	stream.mtx.Unlock()
	if !closing {
		t.Fatal("failed stream was not marked closing before registry lock release")
	}

	select {
	case <-cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("failed stream cleanup did not finish")
	}
	manager.cleanupWG.Wait()
	manager.mu.Lock()
	_, registered := manager.active[key]
	manager.mu.Unlock()
	if registered {
		t.Fatal("failed stream remains registered after cleanup")
	}
}

func TestTorrentDropIntentWaitsForPendingUser(t *testing.T) {
	const hash = "hash"
	manager := &manager{torrentUses: map[string]*torrentUse{
		hash: {refs: 2},
	}}

	manager.releaseTorrentUse(hash, true)
	use := manager.torrentUses[hash]
	if use == nil || use.refs != 1 || !use.dropWhenIdle {
		t.Fatalf("drop intent did not retain pending user: %+v", use)
	}

	manager.releaseTorrentUse(hash, false)
	if _, ok := manager.torrentUses[hash]; ok {
		t.Fatal("torrent use remains after the final pending user released it")
	}
}

func TestUnregisterDoesNotHoldManagerLockWhileTorrentReleaseWaits(t *testing.T) {
	key := streamKey{InfoHash: "hash", Index: 0, Audio: 0, Subtitle: -1}
	stream := &streamInfo{cleanupDone: make(chan struct{})}
	manager := &manager{
		active:      map[streamKey]*streamInfo{key: stream},
		torrentUses: map[string]*torrentUse{key.InfoHash: {refs: 1}},
	}

	manager.torrentMu.Lock()
	torrentLocked := true
	defer func() {
		if torrentLocked {
			manager.torrentMu.Unlock()
		}
	}()
	finished := make(chan struct{})
	go func() {
		manager.unregisterCleanedStream(key, stream)
		close(finished)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		manager.mu.Lock()
		_, registered := manager.active[key]
		manager.mu.Unlock()
		if !registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream registry remained locked behind torrent release")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-finished:
		t.Fatal("torrent release bypassed its lifecycle lock")
	default:
	}

	manager.torrentMu.Unlock()
	torrentLocked = false
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("unregister did not finish after torrent release resumed")
	}
}

func TestMetadataOnlyTorrentUseRemainsReusable(t *testing.T) {
	const hash = "hash"
	manager := &manager{torrentUses: map[string]*torrentUse{
		hash: {refs: 1},
	}}

	manager.releaseTorrentUse(hash, false)
	use := manager.torrentUses[hash]
	if use == nil || use.refs != 0 || use.dropWhenIdle {
		t.Fatalf("metadata-only torrent was not retained for stream preparation: %+v", use)
	}
}

func TestMetadataOnlyTorrentUsesAreBounded(t *testing.T) {
	uses := make(map[string]*torrentUse)
	for index := 0; index <= 16; index++ {
		uses[string(rune('a'+index))] = &torrentUse{lastUsed: time.Unix(int64(index), 0)}
	}
	manager := &manager{torrentUses: uses}

	manager.torrentMu.Lock()
	manager.trimIdleTorrentUsesLocked()
	manager.torrentMu.Unlock()

	if len(manager.torrentUses) != 16 {
		t.Fatalf("idle torrent uses = %d, want 16", len(manager.torrentUses))
	}
	if _, ok := manager.torrentUses["a"]; ok {
		t.Fatal("oldest idle torrent use was not evicted")
	}
}
