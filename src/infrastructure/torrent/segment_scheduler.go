package torrent

import (
	"context"
	"sync"
)

// segmentScheduler owns global job and CPU admission. Foreground work can
// preempt background ownership, while source waiting happens outside CPU
// admission.
type segmentScheduler struct {
	mu            sync.Mutex
	maxJobs       int
	jobs          map[*jobAdmission]struct{}
	maxTranscodes int
	transcodes    map[*transcodeAdmission]struct{}
	waiting       []*transcodeWaiter
}

type jobAdmission struct {
	background bool
	cancel     context.CancelFunc
	released   bool
}

type transcodeAdmission struct {
	background bool
	cancel     context.CancelFunc
	released   bool
}

type transcodeWaiter struct {
	ctx        context.Context
	background bool
	cancel     context.CancelFunc
	ready      chan *transcodeAdmission
}

func newSegmentScheduler(maxJobs, maxTranscodes int) *segmentScheduler {
	return &segmentScheduler{
		maxJobs:       max(1, maxJobs),
		jobs:          make(map[*jobAdmission]struct{}),
		maxTranscodes: max(1, maxTranscodes),
		transcodes:    make(map[*transcodeAdmission]struct{}),
	}
}

func (s *segmentScheduler) reserveJob(background bool, cancel context.CancelFunc) (func(), error) {
	if s == nil {
		return func() {}, nil
	}
	s.mu.Lock()
	if len(s.jobs) >= s.maxJobs && !background {
		for admission := range s.jobs {
			if !admission.background {
				continue
			}
			admission.released = true
			delete(s.jobs, admission)
			if admission.cancel != nil {
				admission.cancel()
			}
			break
		}
	}
	if len(s.jobs) >= s.maxJobs {
		s.mu.Unlock()
		return nil, errStreamJobQueueFull
	}
	admission := &jobAdmission{background: background, cancel: cancel}
	s.jobs[admission] = struct{}{}
	s.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			if !admission.released {
				admission.released = true
				delete(s.jobs, admission)
			}
			s.mu.Unlock()
		})
	}, nil
}

func (s *segmentScheduler) transcode(ctx context.Context, background bool, cancel context.CancelFunc, run func() error) error {
	if s == nil {
		return run()
	}
	admission, err := s.acquireTranscode(ctx, background, cancel)
	if err != nil {
		return err
	}
	defer s.releaseTranscode(admission)
	if err := ctx.Err(); err != nil {
		return err
	}
	return run()
}

func (s *segmentScheduler) acquireTranscode(ctx context.Context, background bool, cancel context.CancelFunc) (*transcodeAdmission, error) {
	waiter := &transcodeWaiter{
		ctx:        ctx,
		background: background,
		cancel:     cancel,
		ready:      make(chan *transcodeAdmission, 1),
	}

	s.mu.Lock()
	s.waiting = append(s.waiting, waiter)
	if !background {
		for admission := range s.transcodes {
			if admission.background && admission.cancel != nil {
				admission.cancel()
				break
			}
		}
	}
	s.dispatchTranscodesLocked()
	s.mu.Unlock()

	select {
	case admission := <-waiter.ready:
		return admission, nil
	case <-ctx.Done():
		s.mu.Lock()
		for index, queued := range s.waiting {
			if queued == waiter {
				s.waiting = append(s.waiting[:index], s.waiting[index+1:]...)
				break
			}
		}
		s.mu.Unlock()
		select {
		case admission := <-waiter.ready:
			s.releaseTranscode(admission)
		default:
		}
		return nil, ctx.Err()
	}
}

func (s *segmentScheduler) releaseTranscode(admission *transcodeAdmission) {
	if s == nil || admission == nil {
		return
	}
	s.mu.Lock()
	if !admission.released {
		admission.released = true
		delete(s.transcodes, admission)
	}
	s.dispatchTranscodesLocked()
	s.mu.Unlock()
}

func (s *segmentScheduler) dispatchTranscodesLocked() {
	for len(s.transcodes) < s.maxTranscodes && len(s.waiting) > 0 {
		index := -1
		for candidate, waiter := range s.waiting {
			if waiter.ctx.Err() != nil {
				continue
			}
			if index < 0 || !waiter.background {
				index = candidate
			}
			if !waiter.background {
				break
			}
		}
		if index < 0 {
			s.waiting = nil
			return
		}
		waiter := s.waiting[index]
		s.waiting = append(s.waiting[:index], s.waiting[index+1:]...)
		admission := &transcodeAdmission{background: waiter.background, cancel: waiter.cancel}
		s.transcodes[admission] = struct{}{}
		waiter.ready <- admission
	}
}
