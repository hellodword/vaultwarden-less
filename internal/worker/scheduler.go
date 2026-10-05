package worker

import (
	"time"

	"github.com/hellodword/vaultwarden-less/internal/routes"
)

const (
	quietWindow      = 30 * time.Second
	maximumWait      = 2 * time.Minute
	ordinaryInterval = 6 * time.Hour
	fullInterval     = 7 * 24 * time.Hour
	sourceRetry      = 5 * time.Minute
)

type task struct {
	Full, ReadAll bool
	Reasons       map[string]uint64
	Events        routes.Summary
}
type schedule struct {
	pending                task
	first, last            time.Time
	urgent                 bool
	nextOrdinary, nextFull time.Time
	retryAt                time.Time
	retryFull              bool
}

func (s *schedule) reason(reason string) {
	if s.pending.Reasons == nil {
		s.pending.Reasons = map[string]uint64{}
	}
	s.pending.Reasons[reason]++
}
func (s *schedule) start(now time.Time) {
	s.nextOrdinary = now.Add(ordinaryInterval)
	s.nextFull = now.Add(fullInterval)
	s.manual(true)
	s.pending.Reasons = map[string]uint64{"startup": 1}
}
func (s *schedule) manual(full bool) {
	s.urgent = true
	s.pending.Full = s.pending.Full || full
	s.pending.ReadAll = s.pending.ReadAll || full
	s.reason("manual")
}
func (s *schedule) event(now time.Time, events routes.Summary) {
	if s.first.IsZero() {
		s.first = now
	}
	s.last = now
	s.pending.Events.Merge(events)
	s.reason("request")
}
func (s *schedule) tick(now time.Time) {
	for !s.nextOrdinary.IsZero() && !s.nextOrdinary.After(now) {
		s.reason("scheduled")
		s.pending.ReadAll = true
		s.urgent = true
		s.nextOrdinary = s.nextOrdinary.Add(ordinaryInterval)
	}
	for !s.nextFull.IsZero() && !s.nextFull.After(now) {
		s.reason("scheduled_full")
		s.pending.Full = true
		s.pending.ReadAll = true
		s.urgent = true
		s.nextFull = s.nextFull.Add(fullInterval)
	}
	if !s.retryAt.IsZero() && !s.retryAt.After(now) {
		s.reason("source_retry")
		s.pending.Full = s.pending.Full || s.retryFull
		s.pending.ReadAll = true
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
	value := s.pending
	value.Full = value.Full || s.retryFull
	value.ReadAll = value.ReadAll || value.Full
	s.pending = task{}
	s.first = time.Time{}
	s.last = time.Time{}
	s.urgent = false
	return value
}
func (s *schedule) complete(now time.Time, full, compared, retry bool) {
	if full && !compared {
		s.retryFull = true
	} else if full && compared {
		s.retryFull = false
	}
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
