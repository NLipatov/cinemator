package torrent

import "context"

import "sync"

// segmentScheduler owns process admission. Session state decides which work is
// useful; the scheduler only enforces global job and transcoder bounds.
type segmentScheduler struct {
	jobs       chan struct{}
	transcodes chan struct{}
}

func newSegmentScheduler(maxJobs, maxTranscodes int) *segmentScheduler {
	return &segmentScheduler{
		jobs:       make(chan struct{}, max(1, maxJobs)),
		transcodes: make(chan struct{}, max(1, maxTranscodes)),
	}
}

func (s *segmentScheduler) reserveJob() (func(), error) {
	if s == nil {
		return func() {}, nil
	}
	select {
	case s.jobs <- struct{}{}:
		var once sync.Once
		return func() {
			once.Do(func() { <-s.jobs })
		}, nil
	default:
		return nil, errStreamJobQueueFull
	}
}

func (s *segmentScheduler) transcode(ctx context.Context, run func() error) error {
	if s == nil {
		return run()
	}
	select {
	case s.transcodes <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-s.transcodes
			return err
		}
		defer func() { <-s.transcodes }()
		return run()
	case <-ctx.Done():
		return ctx.Err()
	}
}
