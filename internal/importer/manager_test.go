package importer

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/deployment"
	"github.com/hellodword/vaultwarden-less/internal/privatefs"
	"github.com/hellodword/vaultwarden-less/internal/snapshot"
	"github.com/hellodword/vaultwarden-less/internal/upstream"
	"github.com/mattn/go-sqlite3"
)

const (
	testCommit  = "0123456789abcdef0123456789abcdef01234567"
	testRelease = "RELEASE.2026-08-28T12-34-56Z-g0123456789ab"
)

var testNow = time.Date(2026, 8, 28, 12, 34, 56, 789_000_000, time.UTC)

type importFixture struct {
	root       string
	state      string
	data       string
	importRoot string
	source     string
	manager    Manager
}

func TestPrepareUsesOnlineBackupWithoutChangingStoppedWALSource(t *testing.T) {
	fixture := newImportFixture(t, false)
	ctx := context.Background()
	before, err := treeID(ctx, fixture.source)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.manager.Prepare(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != PhasePrepared || result.SourceTreeID != before || result.ClientLoginConfirmed {
		t.Fatalf("prepare result = %#v", result)
	}
	after, err := treeID(ctx, fixture.source)
	if err != nil || after != before {
		t.Fatalf("source changed = %q/%q/%v", before, after, err)
	}
	candidate := filepath.Join(fixture.importRoot, "candidate")
	for _, name := range []string{"db.sqlite3-wal", "db.sqlite3-shm", "db.sqlite3-journal", "rsa_key.pem", "rsa_key.pub.pem", "rsa_key.der"} {
		if _, err := os.Lstat(filepath.Join(candidate, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("candidate retained %s: %v", name, err)
		}
	}
	database, err := sql.Open("sqlite3", filepath.Join(candidate, "db.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	var marker string
	if err := database.QueryRow("SELECT value FROM imported_items WHERE id = 2").Scan(&marker); err != nil || marker != "committed-in-wal" {
		t.Fatalf("candidate WAL marker = %q/%v", marker, err)
	}
	if copied, err := os.ReadFile(filepath.Join(candidate, "attachments", "synthetic")); err != nil || string(copied) != "attachment" {
		t.Fatalf("candidate file = %q/%v", copied, err)
	}
	if err := os.Rename(candidate, fixture.manager.candidateTransientPath()); err != nil {
		t.Fatal(err)
	}
	resumed, err := fixture.manager.Prepare(ctx)
	if err != nil || resumed != result {
		t.Fatalf("resume candidate publication = %#v/%v", resumed, err)
	}
	repeated, err := fixture.manager.Prepare(ctx)
	if err != nil || repeated != result {
		t.Fatalf("repeat prepare = %#v/%v", repeated, err)
	}
}

func TestVerifyAcceptAndFinalizeAreResumableAndRotateRSA(t *testing.T) {
	fixture := newImportFixture(t, false)
	ctx := context.Background()
	_, err := fixture.manager.Prepare(ctx)
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(fixture.importRoot, "candidate")
	writePrivateFile(t, filepath.Join(candidate, "rsa_key.pem"), []byte("new-private-key"))
	writePrivateFile(t, filepath.Join(candidate, "rsa_key.pub.pem"), []byte("new-public-key"))
	manager := fixture.manager
	manager.VerifyCandidate = fixedCandidateVerifier
	if _, err := manager.Verify(ctx, false); !IsKind(err, ErrorVerification) {
		t.Fatalf("verify without real client confirmation = %v", err)
	}
	verified, err := manager.Verify(ctx, true)
	if err != nil || verified.Phase != PhaseVerified || !verified.ClientLoginConfirmed {
		t.Fatalf("verify = %#v/%v", verified, err)
	}
	repeatedVerification, err := manager.Verify(ctx, true)
	if err != nil || repeatedVerification != verified {
		t.Fatalf("repeat verify = %#v/%v", repeatedVerification, err)
	}
	accepted, err := manager.Accept(ctx)
	if err != nil || accepted.Phase != PhaseAccepted {
		t.Fatalf("accept = %#v/%v", accepted, err)
	}
	active, err := deployment.RequireActive(fixture.state, deployment.Identity{ProjectCommit: testCommit, Release: testRelease})
	if err != nil || active.ProjectCommit != testCommit {
		t.Fatalf("active = %#v/%v", active, err)
	}
	if empty, err := emptyPrivateDirectory(candidate); err != nil || !empty {
		t.Fatalf("post-exchange candidate = empty %t/%v", empty, err)
	}
	writePrivateFile(t, filepath.Join(fixture.data, "runtime-change"), []byte("legitimate service mutation"))
	if _, err := manager.Accept(ctx); err != nil {
		t.Fatalf("accept did not resume after service mutation: %v", err)
	}
	finalized, err := manager.Finalize(ctx)
	if err != nil || finalized.Phase != PhaseFinalized || finalized.CandidateTreeID != verified.CandidateTreeID {
		t.Fatalf("finalize = %#v/%v", finalized, err)
	}
	if _, err := os.Lstat(candidate); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty candidate survived finalize: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.state, "import", "history", ImportIDInitial)); err != nil {
		t.Fatalf("history is missing: %v", err)
	}
	repeatedFinalized, err := manager.Finalize(ctx)
	if err != nil || repeatedFinalized != finalized {
		t.Fatalf("repeat finalize = %#v/%v", repeatedFinalized, err)
	}
	repeatedAccepted, err := manager.Accept(ctx)
	if err != nil || repeatedAccepted.Phase != PhaseAccepted || repeatedAccepted.CandidateTreeID != verified.CandidateTreeID {
		t.Fatalf("repeat accept after finalize = %#v/%v", repeatedAccepted, err)
	}
	sourceRSA, err := fileID(ctx, fixture.source, "rsa_key.pem")
	if err != nil {
		t.Fatal(err)
	}
	dataRSA, err := fileID(ctx, fixture.data, "rsa_key.pem")
	if err != nil || dataRSA == sourceRSA {
		t.Fatalf("accepted RSA was not rotated: %q/%q/%v", sourceRSA, dataRSA, err)
	}
}

func TestAcceptResumesAfterExchangeAndActivationInterruptions(t *testing.T) {
	for _, failure := range []string{"exchange", "activation"} {
		t.Run(failure, func(t *testing.T) {
			fixture := newImportFixture(t, false)
			ctx := context.Background()
			manager := fixture.manager
			manager.VerifyCandidate = fixedCandidateVerifier
			if _, err := manager.Prepare(ctx); err != nil {
				t.Fatal(err)
			}
			candidate := filepath.Join(fixture.importRoot, "candidate")
			writePrivateFile(t, filepath.Join(candidate, "rsa_key.pem"), []byte("rotated"))
			if _, err := manager.Verify(ctx, true); err != nil {
				t.Fatal(err)
			}
			sentinel := errors.New("synthetic interruption")
			switch failure {
			case "exchange":
				manager.Exchange = func(left, right string) error {
					if err := privatefs.RenameExchange(left, right); err != nil {
						return err
					}
					if left == fixture.data {
						return sentinel
					}
					return nil
				}
			case "activation":
				manager.Activate = func(deployment.Identity, time.Time) error { return sentinel }
			}
			if _, err := manager.Accept(ctx); err == nil {
				t.Fatal("interrupted accept succeeded")
			}
			if _, err := manager.Cancel(ctx); !IsKind(err, ErrorConflict) {
				t.Fatalf("cancel after decision = %v", err)
			}
			resumed := fixture.manager
			resumed.VerifyCandidate = fixedCandidateVerifier
			if _, err := resumed.Accept(ctx); err != nil {
				t.Fatalf("resume accept = %v", err)
			}
			if _, err := deployment.RequireActive(fixture.state, deployment.Identity{ProjectCommit: testCommit, Release: testRelease}); err != nil {
				t.Fatalf("resumed active = %v", err)
			}
		})
	}
}

func TestStateWriteTransientsAreRecoveredWithoutWideningTheCurrentAllowlist(t *testing.T) {
	fixture := newImportFixture(t, false)
	ctx := context.Background()
	manager := fixture.manager
	manager.VerifyCandidate = fixedCandidateVerifier
	if _, err := manager.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, filepath.Join(fixture.importRoot, "candidate", "rsa_key.pem"), []byte("rotated"))
	current := paths(fixture.state).current
	verificationTransient := filepath.Join(current, ".vaultwarden-less-state-"+verificationName+"-interrupted.tmp")
	writePrivateFile(t, verificationTransient, []byte("partial verification"))
	stagingTransient := manager.verificationTransientPath()
	if err := os.Mkdir(stagingTransient, 0o700); err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, filepath.Join(stagingTransient, "partial"), []byte("snapshot scratch"))
	if _, err := manager.Verify(ctx, true); err != nil {
		t.Fatalf("verify after interrupted state write = %v", err)
	}
	if _, err := os.Lstat(verificationTransient); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verification transient survived retry: %v", err)
	}
	if _, err := os.Lstat(stagingTransient); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verification staging transient survived retry: %v", err)
	}

	operatorEntry := filepath.Join(current, ".vaultwarden-less-state-"+decisionName+"-operator")
	writePrivateFile(t, operatorEntry, []byte("not importer-owned"))
	if _, err := manager.Accept(ctx); !IsKind(err, ErrorInvalidState) {
		t.Fatalf("similarly named unexpected entry = %v", err)
	}
	if data, err := os.ReadFile(operatorEntry); err != nil || string(data) != "not importer-owned" {
		t.Fatalf("unexpected entry was removed or changed: %q/%v", data, err)
	}
	if err := os.Remove(operatorEntry); err != nil {
		t.Fatal(err)
	}

	decisionTransient := filepath.Join(current, ".vaultwarden-less-state-"+decisionName+"-interrupted.tmp")
	writePrivateFile(t, decisionTransient, []byte("partial decision"))
	if err := os.Chmod(decisionTransient, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Accept(ctx); !IsKind(err, ErrorInvalidState) {
		t.Fatalf("unsafe reserved transient = %v", err)
	}
	if _, err := os.Lstat(decisionTransient); err != nil {
		t.Fatalf("unsafe reserved transient was removed: %v", err)
	}
	if err := os.Chmod(decisionTransient, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Accept(ctx); err != nil {
		t.Fatalf("accept after interrupted state write = %v", err)
	}
	if _, err := os.Lstat(decisionTransient); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("decision transient survived retry: %v", err)
	}
}

func TestFinalizeResumesAfterEmptyCandidateCleanup(t *testing.T) {
	fixture := newImportFixture(t, false)
	ctx := context.Background()
	manager := fixture.manager
	manager.VerifyCandidate = fixedCandidateVerifier
	if _, err := manager.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, filepath.Join(fixture.importRoot, "candidate", "rsa_key.pem"), []byte("rotated"))
	if _, err := manager.Verify(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Accept(ctx); err != nil {
		t.Fatal(err)
	}
	blockingHistory := filepath.Join(fixture.state, "import", "history", ImportIDInitial)
	if err := os.Mkdir(blockingHistory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Finalize(ctx); !IsKind(err, ErrorConflict) {
		t.Fatalf("blocked finalize = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.importRoot, "candidate")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate cleanup boundary did not complete: %v", err)
	}
	if err := os.Remove(blockingHistory); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Finalize(ctx); err != nil {
		t.Fatalf("resume finalize = %v", err)
	}
}

func TestCancelNeverDeletesSourceOrData(t *testing.T) {
	fixture := newImportFixture(t, false)
	ctx := context.Background()
	operatorDirectory := filepath.Join(fixture.importRoot, ".candidate-operator")
	if err := os.Mkdir(operatorDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, filepath.Join(operatorDirectory, "keep"), []byte("not importer-owned"))
	if _, err := fixture.manager.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	sourceDatabase := filepath.Join(fixture.source, "db.sqlite3")
	before, err := os.ReadFile(sourceDatabase)
	if err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, filepath.Join(fixture.source, "operator-note"), []byte("source may have changed"))
	result, err := fixture.manager.Cancel(ctx)
	if err != nil || result.Phase != PhaseCancelled {
		t.Fatalf("cancel = %#v/%v", result, err)
	}
	after, err := os.ReadFile(sourceDatabase)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("cancel changed source database: %v", err)
	}
	if empty, err := emptyPrivateDirectory(fixture.data); err != nil || !empty {
		t.Fatalf("cancel changed data: %t/%v", empty, err)
	}
	for _, path := range []string{filepath.Join(fixture.importRoot, "candidate"), filepath.Join(fixture.state, "import", "current")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cancel retained disposable path %s: %v", path, err)
		}
	}
	if kept, err := os.ReadFile(filepath.Join(operatorDirectory, "keep")); err != nil || string(kept) != "not importer-owned" {
		t.Fatalf("cancel removed a non-owned candidate-prefix directory: %q/%v", kept, err)
	}
}

func TestPrepareRejectsUnsafeSourceEntriesPermissionsAndDevices(t *testing.T) {
	tests := map[string]func(*testing.T, importFixture, *Manager){
		"symlink": func(t *testing.T, fixture importFixture, _ *Manager) {
			t.Helper()
			if err := os.Symlink("rsa_key.pem", filepath.Join(fixture.source, "unsafe-link")); err != nil {
				t.Fatal(err)
			}
		},
		"hardlink": func(t *testing.T, fixture importFixture, _ *Manager) {
			t.Helper()
			if err := os.Link(filepath.Join(fixture.source, "rsa_key.pem"), filepath.Join(fixture.source, "alias")); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(t *testing.T, fixture importFixture, _ *Manager) {
			t.Helper()
			if err := syscall.Mkfifo(filepath.Join(fixture.source, "pipe"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"permissions": func(t *testing.T, fixture importFixture, _ *Manager) {
			t.Helper()
			if err := os.Chmod(filepath.Join(fixture.source, "rsa_key.pem"), 0o640); err != nil {
				t.Fatal(err)
			}
		},
		"device": func(t *testing.T, fixture importFixture, manager *Manager) {
			t.Helper()
			manager.Device = func(path string) (uint64, error) {
				if path == fixture.source {
					return 2, nil
				}
				return 1, nil
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newImportFixture(t, false)
			manager := fixture.manager
			mutate(t, fixture, &manager)
			if _, err := manager.Prepare(context.Background()); err == nil {
				t.Fatal("unsafe import source was accepted")
			}
			if _, err := os.Lstat(filepath.Join(fixture.importRoot, "candidate")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed prepare published a candidate: %v", err)
			}
		})
	}
}

func TestDefaultVerificationUsesAuditedDatabaseAndSnapshotClosure(t *testing.T) {
	fixture := newImportFixture(t, true)
	ctx := context.Background()
	if _, err := fixture.manager.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(fixture.importRoot, "candidate")
	writePrivateFile(t, filepath.Join(candidate, "rsa_key.pem"), []byte("rotated-current-schema-key"))
	verified, err := fixture.manager.Verify(ctx, true)
	if err != nil || verified.Phase != PhaseVerified {
		t.Fatalf("default verification = %#v/%v", verified, err)
	}
	verification, err := readVerification(paths(fixture.state), mustReadPrepare(t, fixture.state))
	if err != nil {
		t.Fatal(err)
	}
	contract, err := upstream.VaultwardenSchemaContract()
	if err != nil {
		t.Fatal(err)
	}
	if verification.DatabaseMigration != contract.DatabaseMigration || verification.DatabaseSchemaFingerprint != contract.Fingerprint {
		t.Fatalf("verification contract = %#v", verification)
	}
}

func TestImportServingGatesRequirePreparedCandidateAndPublishedDecision(t *testing.T) {
	fixture := newImportFixture(t, false)
	ctx := context.Background()
	manager := fixture.manager
	manager.VerifyCandidate = fixedCandidateVerifier
	if _, err := manager.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	identity := deployment.Identity{ProjectCommit: testCommit, Release: testRelease}
	configured := config.Deployment{
		Root:       "/srv/vaultwarden-less",
		RuntimeUID: uint32(os.Geteuid()),
		RuntimeGID: uint32(os.Getegid()),
		ListenPort: 8080,
		DrillPort:  8081,
	}
	if err := CandidateServingAllowed(ctx, fixture.state, fixture.source, filepath.Join(fixture.importRoot, "candidate"), identity, configured); err != nil {
		t.Fatalf("prepared candidate gate = %v", err)
	}
	database := filepath.Join(fixture.importRoot, "candidate", "db.sqlite3")
	if err := os.Chmod(database, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := CandidateServingAllowed(ctx, fixture.state, fixture.source, filepath.Join(fixture.importRoot, "candidate"), identity, configured); !IsKind(err, ErrorVerification) {
		t.Fatalf("unsafe candidate permission gate = %v", err)
	}
	if err := os.Chmod(database, 0o600); err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, filepath.Join(fixture.importRoot, "candidate", "rsa_key.pem"), []byte("rotated"))
	if _, err := manager.Verify(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := CandidateServingAllowed(ctx, fixture.state, fixture.source, filepath.Join(fixture.importRoot, "candidate"), identity, configured); !IsKind(err, ErrorConflict) {
		t.Fatalf("verified candidate restarted = %v", err)
	}
	if err := NormalServingAllowed(fixture.state, identity); err == nil {
		t.Fatal("normal serving was allowed before accept")
	}
	if _, err := manager.Accept(ctx); err != nil {
		t.Fatal(err)
	}
	if err := NormalServingAllowed(fixture.state, identity); err != nil {
		t.Fatalf("accepted normal serving gate = %v", err)
	}
}

func TestImportStateRejectsNonCanonicalAndUnsafeDocuments(t *testing.T) {
	tests := map[string]func(*testing.T, string){
		"duplicate key": func(t *testing.T, path string) {
			t.Helper()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := bytes.Replace(data, []byte(`"import_id":"initial"`), []byte(`"import_id":"initial","import_id":"initial"`), 1)
			if err := os.WriteFile(path, changed, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"unknown field": func(t *testing.T, path string) {
			t.Helper()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := bytes.Replace(data, []byte("}\n"), []byte(`,"unexpected":true}`+"\n"), 1)
			if err := os.WriteFile(path, changed, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"trailing value": func(t *testing.T, path string) {
			t.Helper()
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString("{}\n"); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		},
		"unsafe mode": func(t *testing.T, path string) {
			t.Helper()
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
		},
		"hard link": func(t *testing.T, path string) {
			t.Helper()
			if err := os.Link(path, path+".alias"); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newImportFixture(t, false)
			if _, err := fixture.manager.Prepare(context.Background()); err != nil {
				t.Fatal(err)
			}
			mutate(t, filepath.Join(fixture.state, "import", "current", prepareName))
			if _, err := fixture.manager.Prepare(context.Background()); !IsKind(err, ErrorInvalidState) {
				t.Fatalf("unsafe import state = %v", err)
			}
		})
	}
}

func newImportFixture(t *testing.T, currentSchema bool) importFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := importFixture{
		root: root, state: filepath.Join(root, "state"), data: filepath.Join(root, "data"),
		importRoot: filepath.Join(root, "import"), source: filepath.Join(root, "import", "source"),
	}
	for _, directory := range []string{fixture.state, fixture.data, fixture.importRoot, fixture.source, filepath.Join(fixture.source, "attachments")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if currentSchema {
		createContractDatabase(t, filepath.Join(fixture.source, "db.sqlite3"))
	} else {
		createStoppedWALDatabase(t, filepath.Join(fixture.source, "db.sqlite3"))
	}
	writePrivateFile(t, filepath.Join(fixture.source, "rsa_key.pem"), []byte("old-private-key"))
	writePrivateFile(t, filepath.Join(fixture.source, "rsa_key.pub.pem"), []byte("old-public-key"))
	writePrivateFile(t, filepath.Join(fixture.source, "attachments", "synthetic"), []byte("attachment"))
	fixture.manager = Manager{
		RootDir: root, StateDir: fixture.state, DataDir: fixture.data, ImportRoot: fixture.importRoot,
		ProjectCommit: testCommit, Release: testRelease, Now: func() time.Time { return testNow },
		OnlineBackup: snapshot.OnlineBackup, VerifyCandidate: auditedCandidateVerifier,
	}
	return fixture
}

func createStoppedWALDatabase(t *testing.T, path string) {
	t.Helper()
	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	connection, err := database.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(context.Background(), "PRAGMA journal_mode = WAL"); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(context.Background(), "PRAGMA wal_autocheckpoint = 0"); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(context.Background(), "CREATE TABLE imported_items(id INTEGER PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(context.Background(), "INSERT INTO imported_items(id, value) VALUES (1, 'base')"); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(context.Background(), "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err := connection.Raw(func(raw any) error {
		sqlite, ok := raw.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("unexpected SQLite driver")
		}
		return sqlite.SetFileControlInt("main", sqlite3.SQLITE_FCNTL_PERSIST_WAL, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(context.Background(), "INSERT INTO imported_items(id, value) VALUES (2, 'committed-in-wal')"); err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		file := path + suffix
		if info, err := os.Lstat(file); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("stopped WAL file %q = %v/%v", suffix, info, err)
		}
		if err := os.Chmod(file, 0o600); err != nil {
			t.Fatal(err)
		}
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
	if _, err := database.Exec("INSERT INTO __diesel_schema_migrations(version, run_on) VALUES (?, '2026-08-28')", contract.DatabaseMigration); err != nil {
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

func fixedCandidateVerifier(ctx context.Context, candidate, _, _ string) (CandidateEvidence, error) {
	if err := ctx.Err(); err != nil {
		return CandidateEvidence{}, err
	}
	database, err := sql.Open("sqlite3", filepath.Join(candidate, "db.sqlite3"))
	if err != nil {
		return CandidateEvidence{}, err
	}
	defer func() { _ = database.Close() }()
	var marker string
	if err := database.QueryRow("SELECT value FROM imported_items WHERE id = 2").Scan(&marker); err != nil || marker != "committed-in-wal" {
		return CandidateEvidence{}, fmt.Errorf("candidate WAL data is missing")
	}
	return CandidateEvidence{
		DatabaseMigration:         "synthetic-migration",
		DatabaseSchemaFingerprint: "sha256:" + strings.Repeat("a", 64),
		ContentID:                 "sha256:" + strings.Repeat("b", 64),
		ArchiveID:                 "sha256:" + strings.Repeat("c", 64),
	}, nil
}

func auditedCandidateVerifier(ctx context.Context, candidate, staging, commit string) (CandidateEvidence, error) {
	if err := snapshot.PrepareStoppedDatabase(ctx, filepath.Join(candidate, "db.sqlite3")); err != nil {
		return CandidateEvidence{}, err
	}
	contract, err := snapshot.ValidateDatabaseContract(ctx, filepath.Join(candidate, "db.sqlite3"))
	if err != nil {
		return CandidateEvidence{}, err
	}
	manifest, err := (snapshot.Builder{DataDir: candidate, StagingDir: staging, ProjectCommit: commit}).Build(ctx)
	if err != nil {
		return CandidateEvidence{}, err
	}
	return CandidateEvidence{
		DatabaseMigration:         contract.Migration,
		DatabaseSchemaFingerprint: contract.SchemaFingerprint,
		ContentID:                 manifest.ContentID,
		ArchiveID:                 manifest.ArchiveID,
	}, nil
}

func mustReadPrepare(t *testing.T, stateDir string) Prepare {
	t.Helper()
	prepare, err := readPrepare(paths(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	return prepare
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
