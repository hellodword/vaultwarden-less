package worker

import (
	"github.com/hellodword/vaultwarden-less/internal/routes"
	"time"
)

const (
	quietWindow      = 30 * time.Second
	maximumWait      = 2 * time.Minute
	ordinaryInterval = 6 * time.Hour
	sourceRetry      = 5 * time.Minute
)

type task struct {
	Generation uint64
	Reasons    map[string]uint64
	Events     routes.Summary
	Operations []string
}
type schedule struct {
	pending               task
	first, last           time.Time
	urgent                bool
	nextOrdinary, retryAt time.Time
}

func (s *schedule) reason(reason string) {
	if s.pending.Reasons == nil {
		s.pending.Reasons = map[string]uint64{}
	}
	s.pending.Reasons[reason]++
}
func (s *schedule) start(now time.Time) {
	if s.nextOrdinary.IsZero() {
		s.nextOrdinary = now.Add(ordinaryInterval)
	}
	s.urgent = true
	s.reason("startup")
}
func (s *schedule) manual() { s.urgent = true; s.reason("manual") }
func (s *schedule) event(now time.Time, events routes.Summary) {
	if s.first.IsZero() {
		s.first = now
	}
	s.last = now
	s.pending.Events.Merge(events)
	s.reason("request")
}
func (s *schedule) tick(now time.Time) {
	if !s.nextOrdinary.IsZero() && !s.nextOrdinary.After(now) {
		s.reason("scheduled")
		s.urgent = true
		s.nextOrdinary = now.Add(ordinaryInterval)
	}
	if !s.retryAt.IsZero() && !s.retryAt.After(now) {
		s.reason("source_retry")
		s.urgent = true
		s.retryAt = time.Time{}
	}
}
func (s *schedule) sourceDue(now time.Time) time.Time {
	if s.urgent {
		return now
	}
	if s.first.IsZero() {
		return time.Time{}
	}
	return earlier(s.last.Add(quietWindow), s.first.Add(maximumWait))
}
func (s *schedule) take() task {
	t := s.pending
	s.pending = task{}
	s.first = time.Time{}
	s.last = time.Time{}
	s.urgent = false
	return t
}
func (s *schedule) complete(now time.Time, retry bool) {
	if retry {
		s.retryAt = now.Add(sourceRetry)
	} else {
		s.retryAt = time.Time{}
	}
}
func earlier(a, b time.Time) time.Time {
	if a.IsZero() {
		return b
	}
	if b.IsZero() || a.Before(b) {
		return a
	}
	return b
}
func targetBackoff(failures int) time.Duration {
	return time.Duration(min(5<<min(max(failures-1, 0), 4), 60)) * time.Minute
}
