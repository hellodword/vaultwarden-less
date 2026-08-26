package worker

import (
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
)

type timer interface {
	C() <-chan time.Time
	Stop() bool
}

type clock interface {
	Now() time.Time
	NewTimer(time.Duration) timer
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

func (realClock) NewTimer(duration time.Duration) timer {
	return realTimer{Timer: time.NewTimer(duration)}
}

type realTimer struct{ *time.Timer }

func (timer realTimer) C() <-chan time.Time { return timer.Timer.C }

type triggerKind string

const (
	triggerDemand   triggerKind = "demand"
	triggerRetry    triggerKind = "retry"
	triggerFallback triggerKind = "fallback"
)

type schedule struct {
	demandFirst *time.Time
	demandLast  time.Time
	retryAt     *time.Time
	fallbackAt  *time.Time
	lastStarted *time.Time
}

func (state *schedule) demand(now time.Time) {
	now = now.UTC()
	if state.demandFirst == nil {
		first := now
		state.demandFirst = &first
	}
	state.demandLast = now
}

func (state *schedule) next() (time.Time, triggerKind, bool) {
	var due time.Time
	var kind triggerKind
	set := func(candidate time.Time, candidateKind triggerKind) {
		if due.IsZero() || candidate.Before(due) || candidate.Equal(due) && candidateKind == triggerDemand {
			due = candidate
			kind = candidateKind
		}
	}
	if state.demandFirst != nil {
		demandDue := state.demandLast.Add(config.QuietWindow)
		maximum := state.demandFirst.Add(config.MaximumQuietWindow)
		if maximum.Before(demandDue) {
			demandDue = maximum
		}
		if state.lastStarted != nil {
			minimum := state.lastStarted.Add(config.MinimumCheckInterval)
			if minimum.After(demandDue) {
				demandDue = minimum
			}
		}
		set(demandDue, triggerDemand)
	}
	if state.retryAt != nil {
		set(*state.retryAt, triggerRetry)
	}
	if state.fallbackAt != nil {
		set(*state.fallbackAt, triggerFallback)
	}
	return due, kind, !due.IsZero()
}

func (state *schedule) begin(now time.Time) {
	now = now.UTC()
	state.lastStarted = &now
	state.demandFirst = nil
	state.demandLast = time.Time{}
	state.retryAt = nil
	state.fallbackAt = nil
}

func (state *schedule) complete(now time.Time, retry bool) {
	now = now.UTC()
	state.retryAt = nil
	state.fallbackAt = nil
	if retry {
		due := now.Add(config.FailureRetry)
		if state.lastStarted != nil {
			minimum := state.lastStarted.Add(config.MinimumCheckInterval)
			if minimum.After(due) {
				due = minimum
			}
		}
		state.retryAt = &due
		return
	}
	due := now.Add(config.FallbackCheckInterval)
	state.fallbackAt = &due
}
