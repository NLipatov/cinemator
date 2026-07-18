package torrent

import (
	"fmt"
	"math"
	"sync"
)

type diskBudget struct {
	path          string
	minFreeBytes  uint64
	minFreeInodes uint64

	mu             sync.Mutex
	reservedBytes  uint64
	reservedInodes uint64
}

type diskReservation struct {
	budget *diskBudget
	bytes  uint64
	inodes uint64
	once   sync.Once
}

type diskAvailability struct {
	bytes  uint64
	inodes uint64
}

func newDiskBudgets(paths []string, minFreeBytes int64, minFreeInodes uint64) (map[string]*diskBudget, error) {
	result := make(map[string]*diskBudget, len(paths))
	byDevice := make(map[string]*diskBudget)
	for _, root := range paths {
		device, err := filesystemDevice(root)
		if err != nil {
			return nil, err
		}
		budget := byDevice[device]
		if budget == nil {
			budget = &diskBudget{
				path:          root,
				minFreeBytes:  uint64(max(int64(0), minFreeBytes)),
				minFreeInodes: minFreeInodes,
			}
			byDevice[device] = budget
		}
		result[root] = budget
	}
	return result, nil
}

func (b *diskBudget) Reserve(bytes, inodes uint64) (*diskReservation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	available, err := filesystemAvailability(b.path)
	if err != nil {
		return nil, err
	}
	requiredBytes, overflow := addUint64(b.minFreeBytes, b.reservedBytes, bytes)
	if overflow || available.bytes < requiredBytes {
		return nil, fmt.Errorf("insufficient disk space: available=%d reserved=%d requested=%d floor=%d", available.bytes, b.reservedBytes, bytes, b.minFreeBytes)
	}
	requiredInodes, overflow := addUint64(b.minFreeInodes, b.reservedInodes, inodes)
	if overflow || available.inodes < requiredInodes {
		return nil, fmt.Errorf("insufficient free inodes: available=%d reserved=%d requested=%d floor=%d", available.inodes, b.reservedInodes, inodes, b.minFreeInodes)
	}
	b.reservedBytes += bytes
	b.reservedInodes += inodes
	return &diskReservation{budget: b, bytes: bytes, inodes: inodes}, nil
}

func (b *diskBudget) CheckFloor() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	available, err := filesystemAvailability(b.path)
	if err != nil {
		return err
	}
	if available.bytes < b.minFreeBytes {
		return fmt.Errorf("disk free-space floor crossed: available=%d floor=%d", available.bytes, b.minFreeBytes)
	}
	if available.inodes < b.minFreeInodes {
		return fmt.Errorf("disk free-inode floor crossed: available=%d floor=%d", available.inodes, b.minFreeInodes)
	}
	return nil
}

func (r *diskReservation) Release() {
	if r == nil || r.budget == nil {
		return
	}
	r.once.Do(func() {
		r.budget.mu.Lock()
		defer r.budget.mu.Unlock()
		if r.budget.reservedBytes < r.bytes || r.budget.reservedInodes < r.inodes {
			panic("released more disk capacity than reserved")
		}
		r.budget.reservedBytes -= r.bytes
		r.budget.reservedInodes -= r.inodes
	})
}

func addUint64(values ...uint64) (uint64, bool) {
	var result uint64
	for _, value := range values {
		if value > math.MaxUint64-result {
			return 0, true
		}
		result += value
	}
	return result, false
}
