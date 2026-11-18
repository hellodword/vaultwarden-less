package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/capture"
	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/restic"
	"github.com/hellodword/vaultwarden-less/internal/testutil"
)

func TestQuietWindowAndMaximumWait(t *testing.T) {
	now := time.Now()
	s := schedule{ordinary: now.Add(ordinaryInterval)}
	for n := 0; n < 10; n++ {
		s.event(now.Add(time.Duration(n) * 15 * time.Second))
	}
	if !s.due().Equal(now.Add(maximumWait)) {
		t.Fatal("continuous writes postponed capture")
	}
	s.take(now.Add(maximumWait))
	s.event(now.Add(3 * time.Minute))
	if !s.due().Equal(now.Add(3*time.Minute + quietWindow)) {
		t.Fatal("quiet window lost")
	}
}

type recoveringRepo struct{ broken atomic.Bool }

func (r *recoveringRepo) Sync(_ context.Context, t config.Target, _ capture.Rule, path, _ string, _ restic.Baseline) (restic.Baseline, error) {
	if t.Name == "remote" && r.broken.Load() {
		return restic.Baseline{}, errors.New("offline")
	}
	bytes, err := os.ReadFile(path)
	return restic.Baseline{ID: string(bytes)}, err
}

func TestIndependentCatchupAndPeriodicCapture(t *testing.T) {
	clock := testutil.NewClock()
	repo := &recoveringRepo{}
	repo.broken.Store(true)
	dir := t.TempDir()
	w := New(&config.Backup{Targets: []config.Target{{Name: "near"}, {Name: "remote"}}}, capture.Builder{CacheDir: dir}, repo, nil)
	w.clock = clock
	version := atomic.Int32{}
	w.source = func(context.Context) (string, error) {
		root, err := os.MkdirTemp(dir, "copy-")
		if err != nil {
			return "", err
		}
		path := filepath.Join(root, "db.sqlite3")
		return path, os.WriteFile(path, fmt.Appendf(nil, "%d", version.Load()), 0600)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	wait := func(a, b string) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			s := w.Status()
			if s.Targets[0].Snapshot == a && s.Targets[1].Snapshot == b && (b != "" || s.Targets[1].Error == "offline") {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("wanted %s/%s, status %+v", a, b, w.Status())
	}
	wait("0", "")
	for i := int32(1); i <= 2; i++ {
		version.Store(i)
		w.Event()
		clock.Advance(quietWindow)
		wait(fmt.Sprint(i), "")
	}
	repo.broken.Store(false)
	version.Store(3)
	clock.Advance(5 * time.Minute)
	wait("3", "3")
	version.Store(4)
	clock.Advance(ordinaryInterval)
	wait("4", "4")
}
