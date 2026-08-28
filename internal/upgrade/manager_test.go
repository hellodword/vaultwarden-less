package upgrade

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/upstream"
	_ "github.com/mattn/go-sqlite3"
)

const (
	testACommit  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testBCommit  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testARelease = "RELEASE.2026-08-27T12-00-00Z-gaaaaaaaaaaaa"
	testBRelease = "RELEASE.2026-08-27T13-00-00Z-gbbbbbbbbbbbb"
)

type managerFixture struct {
	state      string
	data       string
	deployment config.Deployment
	now        time.Time
}

func TestUpgradeAcceptedPathIsDurableAcrossManagerRestarts(t *testing.T) {
	fixture := newManagerFixture(t)
	a := fixture.manager(testACommit, testARelease)
	prepared, err := a.Prepare(testBCommit, testBRelease)
	if err != nil || prepared.Phase != PhasePrepared || len(prepared.Targets) != 0 {
		t.Fatalf("prepare = %#v/%v", prepared, err)
	}
	assertStateModes(t, fixture.state)
	if repeated, err := fixture.manager(testACommit, testARelease).Prepare(testBCommit, testBRelease); err != nil ||
		repeated.SchemaVersion != prepared.SchemaVersion || repeated.UpgradeID != prepared.UpgradeID ||
		repeated.Phase != prepared.Phase || repeated.FromCommit != prepared.FromCommit ||
		repeated.ToCommit != prepared.ToCommit || !equalEvidence(repeated.Targets, prepared.Targets) {
		t.Fatalf("idempotent prepare = %#v/%v", repeated, err)
	}
	if _, err := a.Prepare(strings.Repeat("c", 40), "RELEASE.2026-08-27T14-00-00Z-gcccccccccccc"); !IsKind(err, ErrorConflict) {
		t.Fatalf("different prepare error = %v", err)
	}

	aEvidence := evidence([]string{"primary"}, "1")
	collectCalls := 0
	frozen, err := fixture.manager(testACommit, testARelease).Freeze(context.Background(), []string{"primary"}, func(_ context.Context, commit string, names []string) ([]TargetEvidence, error) {
		collectCalls++
		if commit != testACommit || !equalStrings(names, []string{"primary"}) {
			t.Fatalf("freeze collector identity = %s/%#v", commit, names)
		}
		return aEvidence, nil
	})
	if err != nil || frozen.Phase != PhaseFrozen || collectCalls != 1 {
		t.Fatalf("freeze = %#v/%v calls=%d", frozen, err, collectCalls)
	}
	if _, err := fixture.manager(testACommit, testARelease).Freeze(context.Background(), []string{"primary"}, func(context.Context, string, []string) ([]TargetEvidence, error) {
		t.Fatal("idempotent freeze recollected evidence")
		return nil, nil
	}); err != nil {
		t.Fatalf("idempotent freeze: %v", err)
	}
	if _, err := fixture.manager(testACommit, testARelease).Prepare(testBCommit, testBRelease); !IsKind(err, ErrorConflict) {
		t.Fatalf("prepare repeated after freeze = %v", err)
	}
	if err := ServingAllowed(fixture.state, testACommit, fixture.deployment); !IsKind(err, ErrorConflict) {
		t.Fatalf("frozen A serve = %v", err)
	}
	if err := ServingAllowed(fixture.state, testBCommit, fixture.deployment); !IsKind(err, ErrorConflict) {
		t.Fatalf("frozen B serve = %v", err)
	}

	b := fixture.manager(testBCommit, testBRelease)
	migrated, err := b.MigrateCheck(context.Background())
	if err != nil || migrated.Phase != PhaseMigrated {
		t.Fatalf("migrate = %#v/%v", migrated, err)
	}
	if _, err := fixture.manager(testBCommit, testBRelease).MigrateCheck(context.Background()); !IsKind(err, ErrorConflict) {
		t.Fatalf("repeated migrate error = %v", err)
	}
	bEvidence := evidence([]string{"primary"}, "2")
	verified, err := fixture.manager(testBCommit, testBRelease).Verify(context.Background(), []string{"primary"}, func(_ context.Context, commit string, names []string) ([]TargetEvidence, error) {
		if commit != testBCommit || !equalStrings(names, []string{"primary"}) {
			t.Fatalf("verify collector identity = %s/%#v", commit, names)
		}
		return bEvidence, nil
	})
	if err != nil || verified.Phase != PhaseVerified {
		t.Fatalf("verify = %#v/%v", verified, err)
	}
	if _, err := fixture.manager(testBCommit, testBRelease).Verify(context.Background(), []string{"primary"}, nil); !IsKind(err, ErrorConflict) {
		t.Fatalf("repeated verify error = %v", err)
	}
	if _, err := fixture.manager(testBCommit, testBRelease).Accept(context.Background(), func(context.Context) error {
		return errors.New("drill unavailable")
	}); !IsKind(err, ErrorVerification) {
		t.Fatalf("failed drill accept error = %v", err)
	}
	accepted, err := fixture.manager(testBCommit, testBRelease).Accept(context.Background(), func(context.Context) error { return nil })
	if err != nil || accepted.Phase != PhaseAccepted || len(accepted.Targets) != 0 {
		t.Fatalf("accept = %#v/%v", accepted, err)
	}
	state := paths(fixture.state)
	if _, err := os.Lstat(state.current); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("current remains after accept: %v", err)
	}
	if err := ServingAllowed(fixture.state, testBCommit, fixture.deployment); err != nil {
		t.Fatalf("accepted B serve: %v", err)
	}
	if _, err := fixture.manager(testACommit, testARelease).Rollback(context.Background()); !IsKind(err, ErrorConflict) {
		t.Fatalf("post-accept rollback error = %v", err)
	}
}

func TestPrepareWritesExactComposeEnvironment(t *testing.T) {
	fixture := newManagerFixture(t)
	if _, err := fixture.manager(testACommit, testARelease).Prepare(testBCommit, testBRelease); err != nil {
		t.Fatal(err)
	}
	want := "DEPLOYMENT_SCHEMA_VERSION=1\n" +
		"RUNTIME_UID=" + strconv.Itoa(os.Geteuid()) + "\n" +
		"RUNTIME_GID=" + strconv.Itoa(os.Getegid()) + "\n" +
		"LISTEN_PORT=8080\n" +
		"DRILL_PORT=8081\n" +
		"A_RELEASE=" + testARelease + "\n" +
		"B_RELEASE=" + testBRelease + "\n"
	got, err := os.ReadFile(paths(fixture.state).compose)
	if err != nil || string(got) != want {
		t.Fatalf("compose.env = %q/%v, want %q", got, err, want)
	}
}

func TestExplicitRollbackExchangesAAndQuarantinesOpaqueB(t *testing.T) {
	fixture := newManagerFixture(t)
	a := fixture.manager(testACommit, testARelease)
	if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Freeze(context.Background(), []string{"primary"}, fixedCollector(evidence([]string{"primary"}, "3"))); err != nil {
		t.Fatal(err)
	}
	b := fixture.manager(testBCommit, testBRelease)
	if _, err := b.MigrateCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, filepath.Join(fixture.data, "b-only"), []byte("failed B data"))
	rolledBack, err := fixture.manager(testACommit, testARelease).Rollback(context.Background())
	if err != nil || rolledBack.Phase != PhaseRolledBack || len(rolledBack.Targets) != 0 {
		t.Fatalf("rollback = %#v/%v", rolledBack, err)
	}
	for _, rsa := range []string{"rsa_key.pem", "rsa_key.pub.pem"} {
		if _, err := os.Lstat(filepath.Join(fixture.data, rsa)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("RSA state remains at %s: %v", rsa, err)
		}
	}
	state := paths(fixture.state)
	quarantine := filepath.Join(state.quarantine, upgradeID(testACommit, testBCommit))
	if data, err := os.ReadFile(filepath.Join(quarantine, "data", "b-only")); err != nil || string(data) != "failed B data" {
		t.Fatalf("quarantined B data = %q/%v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(quarantine, candidateName)); err != nil {
		t.Fatalf("opaque candidate was not moved: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(state.history, upgradeID(testACommit, testBCommit), ticketName)); err != nil {
		t.Fatalf("rollback history missing ticket: %v", err)
	}
	if err := ServingAllowed(fixture.state, testACommit, fixture.deployment); err != nil {
		t.Fatalf("rolled-back A serve: %v", err)
	}
}

func TestRollbackDoesNotDecodeOpaqueCandidate(t *testing.T) {
	fixture := newManagerFixture(t)
	a := fixture.manager(testACommit, testARelease)
	if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Freeze(context.Background(), []string{"primary"}, fixedCollector(evidence([]string{"primary"}, "d"))); err != nil {
		t.Fatal(err)
	}
	state := paths(fixture.state)
	opaque := []byte("future B private state; deliberately not JSON\x00\xff\n")
	writePrivateFile(t, state.candidate, opaque)
	writePrivateFile(t, filepath.Join(fixture.data, "b-only"), []byte("failed B data"))

	result, err := fixture.manager(testACommit, testARelease).Rollback(context.Background())
	if err != nil || result.Phase != PhaseRolledBack {
		t.Fatalf("opaque rollback = %#v/%v", result, err)
	}
	quarantined := filepath.Join(state.quarantine, upgradeID(testACommit, testBCommit), candidateName)
	got, err := os.ReadFile(quarantined)
	if err != nil || !bytes.Equal(got, opaque) {
		t.Fatalf("opaque candidate = %q/%v", got, err)
	}
}

func TestRollbackQuarantinesFutureBTreeWithoutTraversingIt(t *testing.T) {
	fixture := newManagerFixture(t)
	a := fixture.manager(testACommit, testARelease)
	if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Freeze(context.Background(), []string{"primary"}, fixedCollector(evidence([]string{"primary"}, "f"))); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(t.TempDir(), "sentinel")
	writePrivateFile(t, sentinel, []byte("must remain untouched"))
	link := filepath.Join(fixture.data, "future-b-link")
	if err := os.Symlink(sentinel, link); err != nil {
		t.Fatal(err)
	}

	result, err := fixture.manager(testACommit, testARelease).Rollback(context.Background())
	if err != nil || result.Phase != PhaseRolledBack {
		t.Fatalf("future-tree rollback = %#v/%v", result, err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "must remain untouched" {
		t.Fatalf("external sentinel = %q/%v", data, err)
	}
	quarantined := filepath.Join(paths(fixture.state).quarantine, upgradeID(testACommit, testBCommit), "data", "future-b-link")
	info, err := os.Lstat(quarantined)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("quarantined future entry = %v/%v", info, err)
	}
	if target, err := os.Readlink(quarantined); err != nil || target != sentinel {
		t.Fatalf("quarantined future link = %q/%v", target, err)
	}
}

func TestUpgradeRejectsCorruptStateWrongOwnerAndCandidateTampering(t *testing.T) {
	t.Run("missing state", func(t *testing.T) {
		fixture := newManagerFixture(t)
		if _, err := fixture.manager(testBCommit, testBRelease).MigrateCheck(context.Background()); !IsKind(err, ErrorConflict) {
			t.Fatalf("missing state error = %v", err)
		}
	})
	t.Run("duplicate prepare key", func(t *testing.T) {
		fixture := newManagerFixture(t)
		a := fixture.manager(testACommit, testARelease)
		if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
			t.Fatal(err)
		}
		state := paths(fixture.state)
		writePrivateFile(t, state.prepare, []byte(`{"schema_version":1,"schema_version":1}`+"\n"))
		called := false
		_, err := a.Freeze(context.Background(), []string{"primary"}, func(context.Context, string, []string) ([]TargetEvidence, error) {
			called = true
			return nil, nil
		})
		if !IsKind(err, ErrorInvalidState) || called {
			t.Fatalf("corrupt prepare = %v/called=%t", err, called)
		}
	})
	t.Run("wrong release owner", func(t *testing.T) {
		fixture := newManagerFixture(t)
		a := fixture.manager(testACommit, testARelease)
		if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Freeze(context.Background(), []string{"primary"}, fixedCollector(evidence([]string{"primary"}, "4"))); err != nil {
			t.Fatal(err)
		}
		wrong := fixture.manager(strings.Repeat("c", 40), "RELEASE.2026-08-27T14-00-00Z-gcccccccccccc")
		if _, err := wrong.MigrateCheck(context.Background()); !IsKind(err, ErrorConflict) {
			t.Fatalf("wrong owner error = %v", err)
		}
	})
	t.Run("candidate unknown field", func(t *testing.T) {
		fixture := newManagerFixture(t)
		prepareAndMigrate(t, fixture)
		state := paths(fixture.state)
		data, err := os.ReadFile(state.candidate)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]any
		if err := json.Unmarshal(data, &object); err != nil {
			t.Fatal(err)
		}
		object["tampered"] = true
		data, _ = json.Marshal(object)
		writePrivateFile(t, state.candidate, append(data, '\n'))
		if _, err := fixture.manager(testBCommit, testBRelease).Verify(context.Background(), []string{"primary"}, fixedCollector(evidence([]string{"primary"}, "5"))); !IsKind(err, ErrorInvalidState) {
			t.Fatalf("tampered candidate error = %v", err)
		}
	})
	t.Run("verification target mismatch", func(t *testing.T) {
		fixture := newManagerFixture(t)
		prepareAndMigrate(t, fixture)
		called := false
		_, err := fixture.manager(testBCommit, testBRelease).Verify(context.Background(), []string{"secondary"}, func(context.Context, string, []string) ([]TargetEvidence, error) {
			called = true
			return evidence([]string{"secondary"}, "9"), nil
		})
		if !IsKind(err, ErrorConflict) || called {
			t.Fatalf("target mismatch = %v/called=%t", err, called)
		}
	})
	t.Run("verified candidate target tampering", func(t *testing.T) {
		fixture := newManagerFixture(t)
		prepareAndMigrate(t, fixture)
		b := fixture.manager(testBCommit, testBRelease)
		if _, err := b.Verify(context.Background(), []string{"primary"}, fixedCollector(evidence([]string{"primary"}, "a"))); err != nil {
			t.Fatal(err)
		}
		state := paths(fixture.state)
		prepare, err := readPrepare(state, fixture.deployment)
		if err != nil {
			t.Fatal(err)
		}
		candidate, err := readCandidate(state, prepare)
		if err != nil {
			t.Fatal(err)
		}
		candidate.Targets = evidence([]string{"secondary"}, "b")
		if err := replaceJSON(state.candidate, candidate); err != nil {
			t.Fatal(err)
		}
		called := false
		_, err = b.Accept(context.Background(), func(context.Context) error {
			called = true
			return nil
		})
		if !IsKind(err, ErrorInvalidState) || called {
			t.Fatalf("candidate target tamper = %v/called=%t", err, called)
		}
	})
}

func TestFreezeFailurePathsDoNotPublishOrExchangeData(t *testing.T) {
	t.Run("missing preparation", func(t *testing.T) {
		fixture := newManagerFixture(t)
		a := fixture.manager(testACommit, testARelease)
		a.PrepareFreeze = nil
		if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
			t.Fatal(err)
		}
		called := false
		_, err := a.Freeze(context.Background(), []string{"primary"}, func(context.Context, string, []string) ([]TargetEvidence, error) {
			called = true
			return evidence([]string{"primary"}, "1"), nil
		})
		if !IsKind(err, ErrorSourceInconsistent) || called {
			t.Fatalf("missing preparation = %v/called=%t", err, called)
		}
		if _, err := os.Lstat(paths(fixture.state).rollback); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rollback tree published after missing preparation: %v", err)
		}
	})
	t.Run("preparation failure", func(t *testing.T) {
		fixture := newManagerFixture(t)
		a := fixture.manager(testACommit, testARelease)
		a.PrepareFreeze = func(context.Context, string) error { return errors.New("database is still live") }
		if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
			t.Fatal(err)
		}
		called := false
		_, err := a.Freeze(context.Background(), []string{"primary"}, func(context.Context, string, []string) ([]TargetEvidence, error) {
			called = true
			return evidence([]string{"primary"}, "2"), nil
		})
		if !IsKind(err, ErrorSourceInconsistent) || called {
			t.Fatalf("preparation failure = %v/called=%t", err, called)
		}
		if _, err := os.Lstat(paths(fixture.state).rollback); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rollback tree published after preparation failure: %v", err)
		}
	})
	t.Run("normalizes stopped tree before evidence", func(t *testing.T) {
		fixture := newManagerFixture(t)
		directory := filepath.Join(fixture.data, "tmp")
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(directory, "runtime")
		if err := os.WriteFile(file, []byte("runtime"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(fixture.data, "rsa_key.pem"), 0o644); err != nil {
			t.Fatal(err)
		}
		a := fixture.manager(testACommit, testARelease)
		if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
			t.Fatal(err)
		}
		called := false
		_, err := a.Freeze(context.Background(), []string{"primary"}, func(context.Context, string, []string) ([]TargetEvidence, error) {
			called = true
			for path, mode := range map[string]os.FileMode{
				directory: 0o700,
				file:      0o600,
				filepath.Join(fixture.data, "rsa_key.pem"): 0o600,
			} {
				if info, statErr := os.Lstat(path); statErr != nil || info.Mode().Perm() != mode {
					t.Fatalf("normalized mode %s = %v/%v", path, info, statErr)
				}
			}
			return evidence([]string{"primary"}, "3"), nil
		})
		if err != nil || !called {
			t.Fatalf("normalized freeze = %v/called=%t", err, called)
		}
	})
	t.Run("collector failure", func(t *testing.T) {
		fixture := newManagerFixture(t)
		a := fixture.manager(testACommit, testARelease)
		if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
			t.Fatal(err)
		}
		before, err := rollbackTreeID(context.Background(), fixture.data)
		if err != nil {
			t.Fatal(err)
		}
		_, err = a.Freeze(context.Background(), []string{"primary"}, func(context.Context, string, []string) ([]TargetEvidence, error) {
			return nil, errors.New("repository unavailable")
		})
		if err == nil {
			t.Fatal("collector failure was accepted")
		}
		after, _ := rollbackTreeID(context.Background(), fixture.data)
		if before != after {
			t.Fatal("active data changed on collector failure")
		}
		state := paths(fixture.state)
		if _, err := os.Lstat(state.rollback); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rollback tree published after failure: %v", err)
		}
	})
	t.Run("live sidecar", func(t *testing.T) {
		fixture := newManagerFixture(t)
		a := fixture.manager(testACommit, testARelease)
		if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
			t.Fatal(err)
		}
		writePrivateFile(t, filepath.Join(fixture.data, "db.sqlite3-wal"), []byte("live"))
		called := false
		_, err := a.Freeze(context.Background(), []string{"primary"}, func(context.Context, string, []string) ([]TargetEvidence, error) {
			called = true
			return evidence([]string{"primary"}, "6"), nil
		})
		if !IsKind(err, ErrorSourceInconsistent) || called {
			t.Fatalf("sidecar freeze = %v/called=%t", err, called)
		}
	})
	t.Run("cross filesystem", func(t *testing.T) {
		fixture := newManagerFixture(t)
		a := fixture.manager(testACommit, testARelease)
		a.Device = func(path string) (uint64, error) {
			if strings.Contains(path, ".rollback-data-") {
				return 2, nil
			}
			return 1, nil
		}
		if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Freeze(context.Background(), []string{"primary"}, fixedCollector(evidence([]string{"primary"}, "7"))); !IsKind(err, ErrorConflict) {
			t.Fatalf("cross-filesystem freeze error = %v", err)
		}
		if _, err := os.Lstat(paths(fixture.state).rollback); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cross-filesystem rollback tree published: %v", err)
		}
	})
}

func TestRollbackPreconditionFailuresNeverExchange(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*testing.T, managerFixture, *Manager)
		wantError ErrorKind
	}{
		{
			name: "tampered ticket",
			mutate: func(t *testing.T, fixture managerFixture, _ *Manager) {
				state := paths(fixture.state)
				data, err := os.ReadFile(state.ticket)
				if err != nil {
					t.Fatal(err)
				}
				var ticket map[string]any
				if err := json.Unmarshal(data, &ticket); err != nil {
					t.Fatal(err)
				}
				ticket["from_commit"] = strings.Repeat("c", 40)
				data, err = json.Marshal(ticket)
				if err != nil {
					t.Fatal(err)
				}
				writePrivateFile(t, state.ticket, append(data, '\n'))
			},
			wantError: ErrorInvalidState,
		},
		{
			name: "tampered rollback tree",
			mutate: func(t *testing.T, fixture managerFixture, _ *Manager) {
				writePrivateFile(t, filepath.Join(fixture.data, "b-only"), []byte("failed B data"))
				writePrivateFile(t, filepath.Join(paths(fixture.state).rollback, "tampered"), []byte("x"))
			},
			wantError: ErrorVerification,
		},
		{
			name: "changed active device",
			mutate: func(_ *testing.T, _ managerFixture, manager *Manager) {
				manager.Device = func(string) (uint64, error) { return ^uint64(0), nil }
			},
			wantError: ErrorConflict,
		},
		{
			name: "conflicting quarantine data",
			mutate: func(t *testing.T, fixture managerFixture, _ *Manager) {
				state := paths(fixture.state)
				quarantine := filepath.Join(state.quarantine, upgradeID(testACommit, testBCommit))
				if err := os.Mkdir(quarantine, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(quarantine, "data"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			wantError: ErrorInvalidState,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newManagerFixture(t)
			a := fixture.manager(testACommit, testARelease)
			if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Freeze(context.Background(), []string{"primary"}, fixedCollector(evidence([]string{"primary"}, "e"))); err != nil {
				t.Fatal(err)
			}
			rollback := fixture.manager(testACommit, testARelease)
			exchanges := 0
			rollback.Exchange = func(string, string) error {
				exchanges++
				return errors.New("exchange must not be called")
			}
			test.mutate(t, fixture, &rollback)
			if _, err := rollback.Rollback(context.Background()); !IsKind(err, test.wantError) || exchanges != 0 {
				t.Fatalf("rollback error/exchanges = %v/%d", err, exchanges)
			}
			if err := ServingAllowed(fixture.state, testACommit, fixture.deployment); !IsKind(err, ErrorConflict) && !IsKind(err, ErrorInvalidState) {
				t.Fatalf("serving gate after failed rollback = %v", err)
			}
		})
	}
}

func TestServingGateAllowsAOnlyBeforeFreezeAndFailsClosed(t *testing.T) {
	fixture := newManagerFixture(t)
	if err := ServingAllowed(fixture.state, testACommit, fixture.deployment); err != nil {
		t.Fatalf("fresh serve = %v", err)
	}
	a := fixture.manager(testACommit, testARelease)
	if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
		t.Fatal(err)
	}
	if err := ServingAllowed(fixture.state, testACommit, fixture.deployment); err != nil {
		t.Fatalf("prepared A serve = %v", err)
	}
	if err := ServingAllowed(fixture.state, testBCommit, fixture.deployment); !IsKind(err, ErrorConflict) {
		t.Fatalf("prepared B serve = %v", err)
	}
	state := paths(fixture.state)
	compose, err := os.ReadFile(state.compose)
	if err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, state.compose, bytes.Replace(compose, []byte("LISTEN_PORT=8080"), []byte("LISTEN_PORT=8082"), 1))
	if err := ServingAllowed(fixture.state, testACommit, fixture.deployment); !IsKind(err, ErrorInvalidState) {
		t.Fatalf("tampered compose serve = %v", err)
	}
	writePrivateFile(t, state.compose, compose)
	writePrivateFile(t, state.ticket, []byte(`{"schema_version":1}`+"\n"))
	if err := ServingAllowed(fixture.state, testACommit, fixture.deployment); !IsKind(err, ErrorInvalidState) {
		t.Fatalf("corrupt frozen serve = %v", err)
	}
}

func TestServingGateRejectsHardLinkedAndOversizedState(t *testing.T) {
	t.Run("hard linked", func(t *testing.T) {
		fixture := newManagerFixture(t)
		if _, err := fixture.manager(testACommit, testARelease).Prepare(testBCommit, testBRelease); err != nil {
			t.Fatal(err)
		}
		state := paths(fixture.state)
		if err := os.Link(state.prepare, filepath.Join(state.current, "prepare-link")); err != nil {
			t.Fatal(err)
		}
		if err := ServingAllowed(fixture.state, testACommit, fixture.deployment); !IsKind(err, ErrorInvalidState) {
			t.Fatalf("hard-linked state serve = %v", err)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		fixture := newManagerFixture(t)
		if _, err := fixture.manager(testACommit, testARelease).Prepare(testBCommit, testBRelease); err != nil {
			t.Fatal(err)
		}
		writePrivateFile(t, paths(fixture.state).prepare, bytes.Repeat([]byte("x"), MaxStateBytes+1))
		if err := ServingAllowed(fixture.state, testACommit, fixture.deployment); !IsKind(err, ErrorInvalidState) {
			t.Fatalf("oversized state serve = %v", err)
		}
	})
}

func newManagerFixture(t *testing.T) managerFixture {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	data := filepath.Join(root, "data")
	for _, directory := range []string{state, data} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	createContractDatabase(t, filepath.Join(data, "db.sqlite3"))
	writePrivateFile(t, filepath.Join(data, "rsa_key.pem"), []byte("private"))
	writePrivateFile(t, filepath.Join(data, "rsa_key.pub.pem"), []byte("public"))
	return managerFixture{
		state: state,
		data:  data,
		deployment: config.Deployment{
			SchemaVersion: config.SchemaVersion,
			RuntimeUID:    uint32(os.Geteuid()),
			RuntimeGID:    uint32(os.Getegid()),
			ListenPort:    8080,
			DrillPort:     8081,
		},
		now: time.Date(2026, 8, 27, 12, 34, 56, 789_000_000, time.UTC),
	}
}

func (fixture managerFixture) manager(commit, release string) Manager {
	return Manager{
		StateDir: fixture.state, DataDir: fixture.data, Deployment: fixture.deployment,
		ProjectCommit: commit, Release: release, Now: func() time.Time { return fixture.now },
		Database: func(ctx context.Context, path string) (DatabaseContract, error) {
			contract, err := snapshot.ValidateDatabaseContract(ctx, path)
			return DatabaseContract(contract), err
		},
		PrepareFreeze: func(context.Context, string) error { return nil },
	}
}

func createContractDatabase(t *testing.T, path string) {
	t.Helper()
	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := upstream.VaultwardenSchemaContract()
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	for _, object := range contract.Objects {
		if _, err := database.Exec(object.SQL); err != nil {
			_ = database.Close()
			t.Fatal(err)
		}
	}
	if _, err := database.Exec("INSERT INTO __diesel_schema_migrations(version, run_on) VALUES (?, '2026-08-27')", contract.DatabaseMigration); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func prepareAndMigrate(t *testing.T, fixture managerFixture) {
	t.Helper()
	a := fixture.manager(testACommit, testARelease)
	if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Freeze(context.Background(), []string{"primary"}, fixedCollector(evidence([]string{"primary"}, "8"))); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.manager(testBCommit, testBRelease).MigrateCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func fixedCollector(value []TargetEvidence) EvidenceCollector {
	return func(context.Context, string, []string) ([]TargetEvidence, error) {
		return value, nil
	}
}

func evidence(names []string, digit string) []TargetEvidence {
	result := make([]TargetEvidence, 0, len(names))
	for index, name := range names {
		value := digit
		if index > 0 {
			value = string(rune('a' + index))
		}
		result = append(result, TargetEvidence{
			Target: name, SnapshotID: strings.Repeat(value, 64),
			ContentID: "sha256:" + strings.Repeat(value, 64), ArchiveID: "sha256:" + strings.Repeat(value, 64),
		})
	}
	return result
}

func writePrivateFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertStateModes(t *testing.T, stateDir string) {
	t.Helper()
	state := paths(stateDir)
	for _, directory := range []string{state.upgrade, state.current, state.history, state.quarantine} {
		if info, err := os.Lstat(directory); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("state directory %s = %v/%v", directory, info, err)
		}
	}
	for _, file := range []string{state.prepare, state.compose} {
		if info, err := os.Lstat(file); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("state file %s = %v/%v", file, info, err)
		}
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalEvidence(left, right []TargetEvidence) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
