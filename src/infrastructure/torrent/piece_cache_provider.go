package torrent

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/anacrolix/missinggo/v2/filecache"
	"github.com/anacrolix/missinggo/v2/resource"
	"github.com/anacrolix/torrent/storage"
)

// pieceCacheProvider disables filecache's independent capacity eviction and
// serializes every open, writer reservation and unlink under one lock.
type pieceCacheProvider struct {
	cache    *filecache.Cache
	base     resource.Provider
	root     string
	capacity int64
	disk     *diskBudget

	mu       sync.Mutex
	leases   map[string]int
	writers  map[string]bool
	retiring map[string]bool
	reserved int64
}

func newPieceCacheProvider(cache *filecache.Cache, root string, capacity int64, disk *diskBudget) *pieceCacheProvider {
	// A negative capacity prevents filecache from unlinking behind our leases.
	cache.SetCapacity(-1)
	return &pieceCacheProvider{
		cache:    cache,
		base:     cache.AsResourceProvider(),
		root:     root,
		capacity: capacity,
		disk:     disk,
		leases:   make(map[string]int),
		writers:  make(map[string]bool),
		retiring: make(map[string]bool),
	}
}

func (p *pieceCacheProvider) NewInstance(location string) (resource.Instance, error) {
	location, err := cleanPieceLocation(location)
	if err != nil {
		return nil, err
	}
	return &pieceCacheInstance{provider: p, location: location}, nil
}

func (p *pieceCacheProvider) ChunksReader(dir string) (storage.PieceReader, error) {
	chunks, err := p.openChunks(strings.TrimSuffix(dir, "/"))
	if err != nil {
		return nil, err
	}
	return chunks, nil
}

func (p *pieceCacheProvider) ReadConsecutiveChunks(prefix string) (io.ReadCloser, error) {
	chunks, err := p.openChunks(strings.TrimSuffix(prefix, "/"))
	if err != nil {
		return nil, err
	}
	return &sequentialPieceChunks{
		Reader: io.NewSectionReader(chunks, 0, chunks.size),
		close:  chunks.Close,
	}, nil
}

func (p *pieceCacheProvider) openChunks(dir string) (*leasedPieceChunks, error) {
	prefix := dir + "/"
	type candidate struct {
		location string
		offset   int64
		size     int64
	}
	var candidates []candidate
	p.mu.Lock()
	p.cache.WalkItems(func(info filecache.ItemInfo) {
		location := string(info.Path)
		if p.retiring[location] || p.writers[location] {
			return
		}
		name, ok := strings.CutPrefix(location, prefix)
		if !ok || name == "" || strings.Contains(name, "/") {
			return
		}
		offset, err := strconv.ParseInt(name, 10, 64)
		if err == nil {
			candidates = append(candidates, candidate{location: location, offset: offset, size: info.Size})
		}
	})
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].offset < candidates[j].offset })

	chunks := &leasedPieceChunks{provider: p}
	for _, candidate := range candidates {
		p.leases[candidate.location]++
		chunks.chunks = append(chunks.chunks, leasedPieceChunk{
			location: candidate.location,
			offset:   candidate.offset,
			size:     candidate.size,
		})
		chunks.size = max(chunks.size, candidate.offset+candidate.size)
	}
	p.mu.Unlock()
	return chunks, nil
}

func (p *pieceCacheProvider) readPinned(location string, data []byte, offset int64) (int, error) {
	p.mu.Lock()
	if p.leases[location] <= 0 {
		p.mu.Unlock()
		return 0, errors.New("torrent piece read lost its lease")
	}
	if _, err := p.statFileLocked(location); err != nil {
		p.mu.Unlock()
		return 0, err
	}
	instance, err := p.base.NewInstance(location)
	if err != nil {
		p.mu.Unlock()
		return 0, err
	}
	raw, err := instance.Get()
	p.mu.Unlock()
	if err != nil {
		return 0, err
	}
	reader, ok := raw.(io.ReaderAt)
	if !ok {
		_ = raw.Close()
		return 0, errors.New("piece cache reader does not support ReaderAt")
	}
	n, readErr := reader.ReadAt(data, offset)
	closeErr := raw.Close()
	if closeErr != nil {
		p.mu.Lock()
		p.leases[location]++
		p.mu.Unlock()
		log.Printf("torrent piece close is ambiguous; keeping file accounted: path=%s err=%v", location, closeErr)
	}
	if readErr == nil {
		readErr = closeErr
	}
	return n, readErr
}

func (p *pieceCacheProvider) open(location string) (io.ReadCloser, error) {
	p.mu.Lock()
	if p.retiring[location] || p.writers[location] {
		p.mu.Unlock()
		return nil, os.ErrNotExist
	}
	if _, err := p.statFileLocked(location); err != nil {
		p.mu.Unlock()
		return nil, err
	}
	instance, err := p.base.NewInstance(location)
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	reader, err := instance.Get()
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	p.leases[location]++
	p.mu.Unlock()
	return &leasedPieceFile{ReadCloser: reader, provider: p, location: location}, nil
}

func (p *pieceCacheProvider) release(location string, closeErr error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if closeErr != nil {
		// The close result is ambiguous. Keep the lease and the named file until
		// process exit rather than risking hidden allocated blocks.
		return
	}
	if p.leases[location] <= 0 {
		panic("released torrent piece without a lease")
	}
	p.leases[location]--
	if p.leases[location] == 0 {
		delete(p.leases, location)
		if p.retiring[location] {
			if err := p.removeLocked(location); err != nil {
				log.Printf("failed to remove retired torrent piece: path=%s err=%v", location, err)
			}
		}
	}
}

func (p *pieceCacheProvider) putSized(location string, reader io.Reader, size int64) error {
	if size < 0 {
		return errors.New("negative piece size")
	}
	reservation, err := p.beginWrite(location, size)
	if err != nil {
		return err
	}
	instance, err := p.base.NewInstance(location)
	if err == nil {
		err = instance.Put(io.LimitReader(reader, size))
	}
	if err == nil {
		info, statErr := p.statFileLocked(location)
		if statErr != nil {
			err = statErr
		} else if info.Size() != size {
			err = fmt.Errorf("piece write size is %d, expected %d", info.Size(), size)
		}
	}
	p.finishWrite(reservation, err)
	return err
}

func (p *pieceCacheProvider) writeAt(location string, data []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errors.New("negative piece offset")
	}
	reservation, err := p.beginWrite(location, offset+int64(len(data)))
	if err != nil {
		return 0, err
	}
	instance, err := p.base.NewInstance(location)
	n := 0
	if err == nil {
		n, err = instance.WriteAt(data, offset)
	}
	p.finishWrite(reservation, err)
	return n, err
}

type pieceWriteReservation struct {
	location string
	bytes    int64
	disk     *diskReservation
}

func (p *pieceCacheProvider) beginWrite(location string, finalSize int64) (pieceWriteReservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.leases[location] != 0 {
		return pieceWriteReservation{}, fmt.Errorf("torrent piece is in use: %s", location)
	}
	if p.retiring[location] {
		return pieceWriteReservation{}, fmt.Errorf("torrent piece is being removed: %s", location)
	}
	current := int64(0)
	inodes := uint64(0)
	if info, err := p.statFileLocked(location); err == nil {
		current = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return pieceWriteReservation{}, err
	} else {
		inodes = 1
	}
	growth := max(int64(0), finalSize-current)
	if err := p.makeRoomLocked(growth, location); err != nil {
		return pieceWriteReservation{}, err
	}
	var diskReservation *diskReservation
	if p.disk != nil {
		reservation, err := p.disk.Reserve(uint64(growth), inodes)
		if err != nil {
			return pieceWriteReservation{}, err
		}
		diskReservation = reservation
	}
	p.reserved += growth
	p.leases[location]++
	p.writers[location] = true
	return pieceWriteReservation{location: location, bytes: growth, disk: diskReservation}, nil
}

func (p *pieceCacheProvider) finishWrite(reservation pieceWriteReservation, writeErr error) {
	defer reservation.disk.Release()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reserved = max(int64(0), p.reserved-reservation.bytes)
	if !p.writers[reservation.location] {
		panic("finished torrent piece write without writer ownership")
	}
	delete(p.writers, reservation.location)
	if p.leases[reservation.location] <= 0 {
		panic("finished torrent piece write without a lease")
	}
	p.leases[reservation.location]--
	if p.leases[reservation.location] == 0 {
		delete(p.leases, reservation.location)
	}
	if (writeErr != nil || p.retiring[reservation.location]) && p.leases[reservation.location] == 0 {
		if err := p.removeLocked(reservation.location); err != nil {
			log.Printf("failed to remove finished torrent piece write: path=%s err=%v", reservation.location, err)
		}
	}
}

func (p *pieceCacheProvider) remove(location string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.leases[location] != 0 {
		p.retiring[location] = true
		return nil
	}
	return p.removeLocked(location)
}

func (p *pieceCacheProvider) removeLocked(location string) error {
	_, err := p.statFileLocked(location)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			delete(p.retiring, location)
			return p.cache.Remove(location)
		}
		return err
	}
	path := p.filePath(location)
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			delete(p.retiring, location)
			return p.cache.Remove(location)
		}
		return err
	}
	// filecache.Remove now only updates the in-memory index and prunes empty
	// directories: the physical unlink above has already succeeded.
	if err := p.cache.Remove(location); err != nil {
		return err
	}
	delete(p.retiring, location)
	return nil
}

func (p *pieceCacheProvider) statFileLocked(location string) (os.FileInfo, error) {
	info, err := os.Lstat(p.filePath(location))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("torrent piece is not a regular file: %s", location)
	}
	if info.Sys() != nil && fileLinkCount(info) != 1 {
		return nil, fmt.Errorf("torrent piece has unexpected link count: %s", location)
	}
	return info, nil
}

func (p *pieceCacheProvider) filePath(location string) string {
	return filepath.Join(p.root, filepath.FromSlash(location))
}

func (p *pieceCacheProvider) trimToCapacity() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.makeRoomLocked(0, "")
}

func (p *pieceCacheProvider) hasLeases() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.leases) != 0
}

func (p *pieceCacheProvider) makeRoomLocked(growth int64, protected string) error {
	if p.capacity <= 0 {
		return nil
	}
	info := p.cache.Info()
	target := p.capacity - p.reserved - growth
	if target < 0 {
		return fmt.Errorf("torrent piece cache cannot reserve %d bytes", growth)
	}
	if info.Filled <= target {
		return nil
	}
	var candidates []filecache.ItemInfo
	p.cache.WalkItems(func(item filecache.ItemInfo) { candidates = append(candidates, item) })
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Accessed.Before(candidates[j].Accessed) })
	filled := info.Filled
	for _, candidate := range candidates {
		if filled <= target {
			break
		}
		location := string(candidate.Path)
		if location == protected || p.leases[location] != 0 {
			continue
		}
		if err := p.removeLocked(location); err != nil {
			continue
		}
		filled -= candidate.Size
	}
	if filled > target {
		return fmt.Errorf("torrent piece cache needs %d more bytes; existing pieces are in use", filled-target)
	}
	return nil
}

type pieceCacheInstance struct {
	provider *pieceCacheProvider
	location string
}

func (i *pieceCacheInstance) Get() (io.ReadCloser, error) {
	return i.provider.open(i.location)
}

func (i *pieceCacheInstance) Put(reader io.Reader) error {
	size, ok := readerSize(reader)
	if !ok {
		return errors.New("torrent piece cache requires a bounded write")
	}
	return i.PutSized(reader, size)
}

func (i *pieceCacheInstance) PutSized(reader io.Reader, size int64) error {
	return i.provider.putSized(i.location, reader, size)
}

func (i *pieceCacheInstance) Stat() (os.FileInfo, error) {
	i.provider.mu.Lock()
	defer i.provider.mu.Unlock()
	if i.provider.retiring[i.location] || i.provider.writers[i.location] {
		return nil, os.ErrNotExist
	}
	return i.provider.statFileLocked(i.location)
}

func (i *pieceCacheInstance) ReadAt(data []byte, offset int64) (int, error) {
	reader, err := i.Get()
	if err != nil {
		return 0, err
	}
	readAt, ok := reader.(io.ReaderAt)
	if !ok {
		_ = reader.Close()
		return 0, errors.New("piece cache reader does not support ReaderAt")
	}
	n, readErr := readAt.ReadAt(data, offset)
	closeErr := reader.Close()
	if readErr == nil {
		readErr = closeErr
	}
	return n, readErr
}

func (i *pieceCacheInstance) WriteAt(data []byte, offset int64) (int, error) {
	return i.provider.writeAt(i.location, data, offset)
}

func (i *pieceCacheInstance) Delete() error {
	return i.provider.remove(i.location)
}

func (i *pieceCacheInstance) Readdirnames() ([]string, error) {
	prefix := i.location + "/"
	var names []string
	i.provider.mu.Lock()
	i.provider.cache.WalkItems(func(info filecache.ItemInfo) {
		location := string(info.Path)
		if i.provider.retiring[location] || i.provider.writers[location] {
			return
		}
		name, ok := strings.CutPrefix(location, prefix)
		if ok && name != "" && !strings.Contains(name, "/") {
			names = append(names, name)
		}
	})
	i.provider.mu.Unlock()
	return names, nil
}

type leasedPieceFile struct {
	io.ReadCloser
	provider *pieceCacheProvider
	location string
	once     sync.Once
	err      error
}

func (f *leasedPieceFile) Close() error {
	f.once.Do(func() {
		f.err = f.ReadCloser.Close()
		f.provider.release(f.location, f.err)
	})
	return f.err
}

func (f *leasedPieceFile) ReadAt(data []byte, offset int64) (int, error) {
	reader, ok := f.ReadCloser.(io.ReaderAt)
	if !ok {
		return 0, errors.New("piece cache reader does not support ReaderAt")
	}
	return reader.ReadAt(data, offset)
}

type leasedPieceChunk struct {
	location string
	offset   int64
	size     int64
}

type leasedPieceChunks struct {
	provider *pieceCacheProvider
	chunks   []leasedPieceChunk
	size     int64
	mu       sync.RWMutex
	closed   bool
	once     sync.Once
}

func (r *leasedPieceChunks) ReadAt(data []byte, offset int64) (int, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return 0, os.ErrClosed
	}
	index := sort.Search(len(r.chunks), func(i int) bool { return r.chunks[i].offset > offset }) - 1
	if index < 0 {
		return 0, io.EOF
	}
	total := 0
	for len(data) != 0 && index < len(r.chunks) {
		chunk := r.chunks[index]
		if chunk.offset > offset {
			return total, io.ErrUnexpectedEOF
		}
		n, err := r.provider.readPinned(chunk.location, data, offset-chunk.offset)
		total += n
		data = data[n:]
		offset += int64(n)
		if len(data) == 0 {
			return total, err
		}
		index++
		if err != nil && !errors.Is(err, io.EOF) {
			return total, err
		}
	}
	if len(data) != 0 {
		return total, io.EOF
	}
	return total, nil
}

func (r *leasedPieceChunks) Close() error {
	r.once.Do(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.closed = true
		for _, chunk := range r.chunks {
			r.provider.release(chunk.location, nil)
		}
	})
	return nil
}

type sequentialPieceChunks struct {
	io.Reader
	close func() error
}

func (r *sequentialPieceChunks) Close() error { return r.close() }

func cleanPieceLocation(location string) (string, error) {
	clean := strings.TrimPrefix(path.Clean("/"+location), "/")
	if clean == "" || clean == "." {
		return "", errors.New("bad torrent piece cache path")
	}
	return clean, nil
}

func readerSize(reader io.Reader) (int64, bool) {
	switch reader := reader.(type) {
	case interface{ Size() int64 }:
		return reader.Size(), true
	case interface{ Len() int }:
		return int64(reader.Len()), true
	default:
		return 0, false
	}
}

var (
	_ resource.Provider              = (*pieceCacheProvider)(nil)
	_ storage.ChunksReaderer         = (*pieceCacheProvider)(nil)
	_ storage.ConsecutiveChunkReader = (*pieceCacheProvider)(nil)
	_ resource.Instance              = (*pieceCacheInstance)(nil)
	_ storage.SizedPutter            = (*pieceCacheInstance)(nil)
	_ io.ReaderAt                    = (*leasedPieceFile)(nil)
	_ storage.PieceReader            = (*leasedPieceChunks)(nil)
	_ io.ReadCloser                  = (*sequentialPieceChunks)(nil)
)
