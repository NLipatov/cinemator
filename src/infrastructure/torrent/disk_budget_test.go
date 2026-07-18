package torrent

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestDiskBudgetPreservesConfiguredFloors(t *testing.T) {
	root := t.TempDir()
	available, err := filesystemAvailability(root)
	if err != nil {
		t.Fatal(err)
	}
	budget := &diskBudget{path: root, minFreeBytes: available.bytes, minFreeInodes: available.inodes}
	if _, err := budget.Reserve(1, 0); err == nil {
		t.Fatal("byte reservation crossed the free-space floor")
	}
	if _, err := budget.Reserve(0, 1); err == nil {
		t.Fatal("inode reservation crossed the free-inode floor")
	}
}

func TestDiskBudgetsShareAuthorityOnOneFilesystem(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	budgets, err := newDiskBudgets([]string{first, second}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if budgets[first] != budgets[second] {
		t.Fatal("paths on one filesystem received independent reservation authorities")
	}
}

func TestDiskBudgetSerializesConcurrentReservations(t *testing.T) {
	root := t.TempDir()
	available, err := filesystemAvailability(root)
	if err != nil {
		t.Fatal(err)
	}
	budget := &diskBudget{path: root}
	request := available.bytes/4*3 + 1
	start := make(chan struct{})
	var successes atomic.Int32
	var reservationsMu sync.Mutex
	var reservations []*diskReservation
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			reservation, err := budget.Reserve(request, 0)
			if err == nil {
				successes.Add(1)
				reservationsMu.Lock()
				reservations = append(reservations, reservation)
				reservationsMu.Unlock()
			}
		}()
	}
	close(start)
	workers.Wait()
	for _, reservation := range reservations {
		reservation.Release()
	}
	if got := successes.Load(); got != 1 {
		t.Fatalf("successful concurrent reservations = %d, want 1", got)
	}
}
