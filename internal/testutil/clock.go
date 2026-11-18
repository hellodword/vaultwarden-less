// Package testutil provides a deterministic clock for concurrency policy tests.
package testutil

import (
	"sync"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/timing"
)

type Clock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*timer
}
type timer struct {
	clock   *Clock
	at      time.Time
	ch      chan time.Time
	stopped bool
}

func NewClock() *Clock          { return &Clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }
func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *Clock) NewTimerAt(at time.Time) timing.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &timer{clock: c, at: at, ch: make(chan time.Time, 1)}
	if !t.at.After(c.now) {
		t.ch <- c.now
		t.stopped = true
	} else {
		c.timers = append(c.timers, t)
	}
	return t
}

// HasTimer lets a test advance only after the worker has scheduled a deadline.
func (c *Clock) HasTimer(at time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, timer := range c.timers {
		if !timer.stopped && timer.at.Equal(at) {
			return true
		}
	}
	return false
}
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	remaining := c.timers[:0]
	for _, t := range c.timers {
		if t.stopped {
			continue
		}
		if !t.at.After(c.now) {
			t.ch <- c.now
			t.stopped = true
		} else {
			remaining = append(remaining, t)
		}
	}
	c.timers = remaining
}
func (t *timer) C() <-chan time.Time { return t.ch }
func (t *timer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}
