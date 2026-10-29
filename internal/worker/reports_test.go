package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/capture"
	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/notify"
	"github.com/hellodword/vaultwarden-less/internal/state"
)

type reportRecorder struct{ events []notify.Event }

func (r *reportRecorder) Enqueue(e notify.Event) error { r.events = append(r.events, e); return nil }
func (*reportRecorder) Status() []notify.Status        { return nil }

func TestReportsSurviveRestartAndConsolidateMissedDailyPeriods(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Backup{Notifications: config.Notifications{Timezone: "Asia/Shanghai", Location: location, DailyAt: "21:00", ReminderEvery: 6 * time.Hour}}
	reports := &reportRecorder{}
	w := New(cfg, capture.Builder{}, nil, reports, nil)
	w.store = state.Store{Directory: t.TempDir()}
	w.status.Source = "complete"
	if err := w.report(now); err != nil {
		t.Fatal(err)
	}
	if len(reports.events) != 0 {
		t.Fatal("healthy source generated a message")
	}
	w.status.Source = "incomplete"
	w.status.Issues = map[string]int{"missing_file": 1}
	if err := w.report(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(reports.events) != 1 || reports.events[0].Kind != "Fault" {
		t.Fatal("initial fault absent")
	}
	w.status.Issues["missing_file"] = 2
	if err := w.report(now.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(reports.events) != 2 || reports.events[1].Kind != "Fault" {
		t.Fatal("expanded fault absent")
	}
	if err := w.persist(); err != nil {
		t.Fatal(err)
	}
	restarted := New(cfg, capture.Builder{}, nil, reports, nil)
	if err := restarted.Restore(w.store); err != nil {
		t.Fatal(err)
	}
	if err := restarted.report(now.Add(48 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(reports.events) != 4 || reports.events[2].Kind != "Reminder" || reports.events[3].Kind != "Daily" {
		t.Fatalf("catch-up reports: %+v", reports.events)
	}
	daily := reports.events[3]
	if !daily.PeriodStart.Equal(now) || !daily.PeriodEnd.Equal(now.Add(48*time.Hour)) {
		t.Fatal("missed daily interval was lost")
	}
	if err := restarted.report(now.Add(48*time.Hour + time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(reports.events) != 4 {
		t.Fatal("healthy internal check restarted reporting")
	}
	restarted.status.Source = "complete"
	restarted.status.Issues = nil
	if err := restarted.report(now.Add(48*time.Hour + 2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	last := reports.events[len(reports.events)-1]
	if last.Kind != "Recovery" || last.Faults["source"].Recovered.IsZero() || !last.Faults["source"].First.Equal(now.Add(time.Minute)) {
		t.Fatal("recovery lost persisted fault history")
	}
}

func TestAcceptedHTTPJobOutlivesRequestCancellation(t *testing.T) {
	w, _, _, _ := setupWorker(t, "one")
	runWorker(t, w)
	waitFor(t, func() bool { return settled(w) })
	requestContext, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, BackupPath, strings.NewReader(`{"force":true}`)).WithContext(requestContext)
	response := httptest.NewRecorder()
	Handler(w).ServeHTTP(response, request)
	cancel()
	if response.Code != http.StatusAccepted {
		t.Fatalf("acceptance: %d", response.Code)
	}
	var receipt Operation
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { result, _ := w.Operation(receipt.ID); return result.Done() })
	result, _ := w.Operation(receipt.ID)
	if result.State != "succeeded" || len(result.Targets) != 1 || result.Targets[0].State != "uploaded" {
		t.Fatalf("accepted job canceled: %+v", result)
	}
}
