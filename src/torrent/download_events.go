package torrent

import (
	"context"
	"sync"
)

type downloadEventBroadcaster struct {
	mu          sync.Mutex
	subscribers map[chan struct{}]struct{}
}

func newDownloadEventBroadcaster() *downloadEventBroadcaster {
	return &downloadEventBroadcaster{
		subscribers: make(map[chan struct{}]struct{}),
	}
}

func (b *downloadEventBroadcaster) subscribe(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.mu.Lock()
		if _, ok := b.subscribers[ch]; ok {
			delete(b.subscribers, ch)
			close(ch)
		}
		b.mu.Unlock()
	}()

	return ch
}

func (b *downloadEventBroadcaster) notify() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
