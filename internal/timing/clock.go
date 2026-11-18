package timing

import "time"

type Timer interface {
	C() <-chan time.Time
	Stop() bool
}
type Clock interface {
	Now() time.Time
	NewTimerAt(time.Time) Timer
}
type Real struct{}

func (Real) Now() time.Time                { return time.Now().UTC() }
func (Real) NewTimerAt(at time.Time) Timer { return realTimer{time.NewTimer(time.Until(at))} }

type realTimer struct{ *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.Timer.C }
