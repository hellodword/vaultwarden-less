package worker

import (
	"github.com/hellodword/vaultwarden-less/internal/capture"
	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/hellodword/vaultwarden-less/internal/state"
	"testing"
	"time"
)

type recorder []notify.Event

func (*recorder) Status() []notify.Status { return nil }

func (r *recorder) Enqueue(e notify.Event) error { *r = append(*r, e); return nil }
func TestReportsSurviveRestartWithoutInventingRecovery(t *testing.T) {
	cfg, err := config.DecodeBackup([]byte("targets: []"))
	if err != nil {
		t.Fatal(err)
	}
	var events recorder
	store := state.Store{Directory: t.TempDir()}
	makeWorker := func() *Worker {
		w := New(cfg, capture.Builder{}, nil, &events)
		if err := w.Restore(store); err != nil {
			t.Fatal(err)
		}
		return w
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	w := makeWorker()
	w.status.Source, w.status.SourceError = "failed", "capture failed"
	if err := w.report(now); err != nil {
		t.Fatal(err)
	}
	w = makeWorker()
	if err := w.report(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	w.status.Source, w.status.SourceError = "failed", "capture failed"
	if err := w.report(now.Add(24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	w.status.Source, w.status.SourceError = "ready", ""
	if err := w.report(now.Add(24*time.Hour + time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatal(events)
	}
	for i, k := range []string{"Fault", "Reminder", "Daily", "Recovery"} {
		if events[i].Kind != k {
			t.Fatal(events)
		}
	}
	if !events[3].Faults["source"].First.Equal(now) {
		t.Fatal("original fault time lost")
	}
}
