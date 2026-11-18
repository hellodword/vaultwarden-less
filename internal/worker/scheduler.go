package worker

import "time"

const (
	quietWindow      = 30 * time.Second
	maximumWait      = 2 * time.Minute
	ordinaryInterval = 6 * time.Hour
	sourceRetry      = 5 * time.Minute
)

type schedule struct{ first, last, ordinary, retry time.Time }

func (s *schedule) event(now time.Time) {
	if s.first.IsZero() {
		s.first = now
	}
	s.last = now
}
func (s schedule) due() time.Time {
	d := earlier(s.ordinary, s.retry)
	if !s.first.IsZero() {
		d = earlier(d, earlier(s.last.Add(quietWindow), s.first.Add(maximumWait)))
	}
	return d
}
func (s *schedule) take(now time.Time) {
	if !s.ordinary.After(now) {
		s.ordinary = now.Add(ordinaryInterval)
	}
	s.first, s.last, s.retry = time.Time{}, time.Time{}, time.Time{}
}
func earlier(a, b time.Time) time.Time {
	if a.IsZero() || !b.IsZero() && b.Before(a) {
		return b
	}
	return a
}
func targetBackoff(failures int) time.Duration {
	return time.Duration(min(5<<min(max(failures-1, 0), 4), 60)) * time.Minute
}
