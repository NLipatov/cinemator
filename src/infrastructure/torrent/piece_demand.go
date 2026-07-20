package torrent

import (
	"sync"

	"github.com/anacrolix/torrent"
)

// pieceDemand reference-counts manager-owned priorities that must outlive a
// transient range reader. Reader and file priorities remain independent.
type pieceDemand struct {
	mu   sync.Mutex
	refs map[pieceDemandKey]int
}

type pieceDemandKey struct {
	torrent *torrent.Torrent
	index   int
}

func newPieceDemand() *pieceDemand {
	return &pieceDemand{refs: make(map[pieceDemandKey]int)}
}

func (d *pieceDemand) acquire(t *torrent.Torrent, begin, end int) func() {
	if d == nil || t == nil || begin >= end {
		return func() {}
	}
	d.mu.Lock()
	keys := make([]pieceDemandKey, 0, end-begin)
	for index := begin; index < end; index++ {
		key := pieceDemandKey{torrent: t, index: index}
		if d.refs[key] == 0 {
			t.Piece(index).SetPriority(torrent.PiecePriorityNormal)
		}
		d.refs[key]++
		keys = append(keys, key)
	}
	d.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			for _, key := range keys {
				count := d.refs[key] - 1
				if count > 0 {
					d.refs[key] = count
					continue
				}
				delete(d.refs, key)
				key.torrent.Piece(key.index).SetPriority(torrent.PiecePriorityNone)
			}
			d.mu.Unlock()
		})
	}
}
