package torrent

import (
	"context"
	"sync"
)

// segmentScheduler owns global job admission and independent bounded worker
// lanes. Foreground work can preempt background ownership within its lane;
// source waiting happens after the worker admission has been released.
type segmentScheduler struct {
	mu          sync.Mutex
	maxJobs     int
	jobs        map[*jobAdmission]struct{}
	jobsChanged chan struct{}
	encoders    *priorityWorkerPool
	packagers   *priorityWorkerPool
}

type jobAdmission struct {
	background bool
	cancel     context.CancelFunc
	released   bool
}

type workerAdmission struct {
	background bool
	cancel     context.CancelFunc
	released   bool
}

type workerWaiter struct {
	ctx        context.Context
	background bool
	cancel     context.CancelFunc
	ready      chan *workerAdmission
}

type priorityWorkerPool struct {
	mu      sync.Mutex
	maximum int
	active  map[*workerAdmission]struct{}
	waiting []*workerWaiter
}

func newSegmentScheduler(maxJobs, maxTranscodes, maxPackagers int) *segmentScheduler {
	return &segmentScheduler{
		maxJobs:     max(1, maxJobs),
		jobs:        make(map[*jobAdmission]struct{}),
		jobsChanged: make(chan struct{}),
		encoders:    newPriorityWorkerPool(maxTranscodes),
		packagers:   newPriorityWorkerPool(maxPackagers),
	}
}

func (s *segmentScheduler) signalJobChangeLocked() {
	close(s.jobsChanged)
	s.jobsChanged = make(chan struct{})
}

func (s *segmentScheduler) jobChanges() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobsChanged
}

func waitForJobChange(ctx context.Context, changed <-chan struct{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed:
		return nil
	}
}

func (s *segmentScheduler) waitForJobCapacity(ctx context.Context) error {
	s.mu.Lock()
	if len(s.jobs) < s.maxJobs {
		s.mu.Unlock()
		return nil
	}
	changed := s.jobsChanged
	s.mu.Unlock()
	return waitForJobChange(ctx, changed)
}

func newPriorityWorkerPool(maximum int) *priorityWorkerPool {
	return &priorityWorkerPool{
		maximum: max(1, maximum),
		active:  make(map[*workerAdmission]struct{}),
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
				s.signalJobChangeLocked()
			}
			s.mu.Unlock()
		})
	}, nil
}

func (s *segmentScheduler) transcode(ctx context.Context, background bool, cancel context.CancelFunc, run func() error) error {
	if s == nil {
		return run()
	}
	return s.encoders.run(ctx, background, cancel, run)
}

func (s *segmentScheduler) packageMedia(ctx context.Context, background bool, cancel context.CancelFunc, run func() error) error {
	if s == nil {
		return run()
	}
	return s.packagers.run(ctx, background, cancel, run)
}

func (p *priorityWorkerPool) run(ctx context.Context, background bool, cancel context.CancelFunc, run func() error) error {
	admission, err := p.acquire(ctx, background, cancel)
	if err != nil {
		return err
	}
	defer p.release(admission)
	if err := ctx.Err(); err != nil {
		return err
	}
	return run()
}

func (p *priorityWorkerPool) acquire(ctx context.Context, background bool, cancel context.CancelFunc) (*workerAdmission, error) {
	waiter := &workerWaiter{
		ctx:        ctx,
		background: background,
		cancel:     cancel,
		ready:      make(chan *workerAdmission, 1),
	}

	p.mu.Lock()
	p.waiting = append(p.waiting, waiter)
	if !background {
		for admission := range p.active {
			if admission.background && admission.cancel != nil {
				admission.cancel()
				break
			}
		}
	}
	p.dispatchLocked()
	p.mu.Unlock()

	select {
	case admission := <-waiter.ready:
		return admission, nil
	case <-ctx.Done():
		p.mu.Lock()
		for index, queued := range p.waiting {
			if queued == waiter {
				p.waiting = append(p.waiting[:index], p.waiting[index+1:]...)
				break
			}
		}
		p.mu.Unlock()
		select {
		case admission := <-waiter.ready:
			p.release(admission)
		default:
		}
		return nil, ctx.Err()
	}
}

func (p *priorityWorkerPool) release(admission *workerAdmission) {
	if admission == nil {
		return
	}
	p.mu.Lock()
	if !admission.released {
		admission.released = true
		delete(p.active, admission)
	}
	p.dispatchLocked()
	p.mu.Unlock()
}

func (p *priorityWorkerPool) dispatchLocked() {
	for len(p.active) < p.maximum && len(p.waiting) > 0 {
		index := -1
		for candidate, waiter := range p.waiting {
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
			p.waiting = nil
			return
		}
		waiter := p.waiting[index]
		p.waiting = append(p.waiting[:index], p.waiting[index+1:]...)
		admission := &workerAdmission{background: waiter.background, cancel: waiter.cancel}
		p.active[admission] = struct{}{}
		waiter.ready <- admission
	}
}
