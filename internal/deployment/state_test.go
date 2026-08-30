package deployment

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	testCommit  = "0123456789abcdef0123456789abcdef01234567"
	testRelease = "RELEASE.2026-08-28T12-34-56Z-g0123456789ab"
)

var testNow = time.Date(2026, 8, 28, 12, 34, 56, 789_000_000, time.UTC)

func TestInitPublishesExactActiveIdentityAndIsIdempotent(t *testing.T) {
	manager := deploymentFixture(t)
	manager.Now = func() time.Time { return testNow }
	result, err := manager.Init()
	if err != nil || result != initResult(StatusInitialized, Identity{ProjectCommit: testCommit, Release: testRelease}) {
		t.Fatalf("initialize = %#v/%v", result, err)
	}

	activePath := filepath.Join(manager.StateDir, "deployment", "active.json")
	want := `{"project_commit":"` + testCommit + `","release":"` + testRelease + `","activated_at":"2026-08-28T12:34:56.789Z"}` + "\n"
	data, err := os.ReadFile(activePath)
	if err != nil || string(data) != want {
		t.Fatalf("active bytes = %q/%v", data, err)
	}
	info, err := os.Lstat(activePath)
	stat, ok := info.Sys().(*syscall.Stat_t)
	if err != nil || !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || stat.Nlink != 1 ||
		stat.Uid != uint32(os.Geteuid()) || stat.Gid != uint32(os.Getegid()) {
		t.Fatalf("active metadata = %#v/%#v/%v", info, stat, err)
	}
	deploymentInfo, err := os.Lstat(filepath.Dir(activePath))
	if err != nil || !deploymentInfo.IsDir() || deploymentInfo.Mode().Perm() != 0o700 {
		t.Fatalf("deployment directory = %#v/%v", deploymentInfo, err)
	}

	if err := os.WriteFile(filepath.Join(manager.DataDir, "created-after-init"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager.Now = func() time.Time { return testNow.Add(time.Hour) }
	repeated, err := manager.Init()
	if err != nil || repeated.Status != StatusAlreadyInitialized {
		t.Fatalf("repeat initialize = %#v/%v", repeated, err)
	}
	after, err := os.ReadFile(activePath)
	if err != nil || string(after) != want {
		t.Fatalf("repeated init rewrote active = %q/%v", after, err)
	}

	manager.ProjectCommit = strings.Repeat("f", 40)
	manager.Release = "RELEASE.2026-08-28T13-34-56Z-gffffffffffff"
	if _, err := manager.Init(); !IsKind(err, ErrorConflict) {
		t.Fatalf("conflicting initialize error = %v", err)
	}
}

func TestInitRejectsNonemptyDataAndCurrentWorkflows(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, manager Manager)
	}{
		{
			name: "nonempty data",
			setup: func(t *testing.T, manager Manager) {
				if err := os.WriteFile(filepath.Join(manager.DataDir, "db.sqlite3"), []byte("synthetic"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "upgrade current",
			setup: func(t *testing.T, manager Manager) {
				if err := os.MkdirAll(filepath.Join(manager.StateDir, "upgrade", "current"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "import current symlink",
			setup: func(t *testing.T, manager Manager) {
				if err := os.Mkdir(filepath.Join(manager.StateDir, "import"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(manager.DataDir, filepath.Join(manager.StateDir, "import", "current")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := deploymentFixture(t)
			test.setup(t, manager)
			if _, err := manager.Init(); !IsKind(err, ErrorConflict) {
				t.Fatalf("initialize error = %v", err)
			}
			if _, err := os.Lstat(filepath.Join(manager.StateDir, "deployment", "active.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("active state created after rejected initialization: %v", err)
			}
		})
	}
}

func TestInitRejectsUnsafeExistingDeploymentDirectoryWithoutNormalizingIt(t *testing.T) {
	manager := deploymentFixture(t)
	directory := filepath.Join(manager.StateDir, "deployment")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Init(); !IsKind(err, ErrorInvalidState) {
		t.Fatalf("unsafe deployment directory error = %v", err)
	}
	info, err := os.Lstat(directory)
	if err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("unsafe deployment directory was normalized = %#v/%v", info, err)
	}
}

func TestActiveStrictJSONAndIdentityValidation(t *testing.T) {
	canonical := `{"project_commit":"` + testCommit + `","release":"` + testRelease + `","activated_at":"2026-08-28T12:34:56.789Z"}` + "\n"
	retiredVersionField := "schema_" + "version"
	mutations := map[string]string{
		"empty":              "",
		"unknown field":      strings.TrimSuffix(canonical, "}\n") + `,"extra":true}` + "\n",
		"duplicate key":      strings.Replace(canonical, `"project_commit":"`+testCommit+`"`, `"project_commit":"`+testCommit+`","project_commit":"`+testCommit+`"`, 1),
		"trailing value":     canonical + "{}\n",
		"wrong type":         strings.Replace(canonical, `"project_commit":"`+testCommit+`"`, `"project_commit":1`, 1),
		"retired field":      strings.Replace(canonical, "{", `{"`+retiredVersionField+`":1,`, 1),
		"noncanonical":       " " + canonical,
		"uppercase commit":   strings.Replace(canonical, testCommit, strings.ToUpper(testCommit), 1),
		"mismatched release": strings.Replace(canonical, testRelease, "RELEASE.2026-08-28T12-34-56Z-gffffffffffff", 1),
		"timestamp offset":   strings.Replace(canonical, "2026-08-28T12:34:56.789Z", "2026-08-28T12:34:56.789+00:00", 1),
		"missing millis":     strings.Replace(canonical, "2026-08-28T12:34:56.789Z", "2026-08-28T12:34:56Z", 1),
	}
	for name, contents := range mutations {
		t.Run(name, func(t *testing.T) {
			manager := deploymentFixture(t)
			writeActiveFixture(t, manager.StateDir, []byte(contents))
			if _, err := RequireActive(manager.StateDir, Identity{ProjectCommit: testCommit, Release: testRelease}); !IsKind(err, ErrorInvalidState) {
				t.Fatalf("strict active error = %v", err)
			}
		})
	}

	t.Run("invalid UTF-8", func(t *testing.T) {
		manager := deploymentFixture(t)
		writeActiveFixture(t, manager.StateDir, []byte{0xff})
		if _, err := ReadActive(manager.StateDir); !IsKind(err, ErrorInvalidState) {
			t.Fatalf("invalid UTF-8 error = %v", err)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		manager := deploymentFixture(t)
		writeActiveFixture(t, manager.StateDir, []byte(strings.Repeat("x", MaxStateBytes+1)))
		if _, err := ReadActive(manager.StateDir); !IsKind(err, ErrorInvalidState) {
			t.Fatalf("oversized active error = %v", err)
		}
	})
}

func TestActiveRejectsUnsafeFilesystemObjectsAndMismatch(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		manager := deploymentFixture(t)
		if _, err := ReadActive(manager.StateDir); !IsKind(err, ErrorNotInitialized) {
			t.Fatalf("missing active error = %v", err)
		}
	})

	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, path string)
	}{
		{
			name: "wrong mode",
			mutate: func(t *testing.T, path string) {
				if err := os.Chmod(path, 0o640); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "hard link alias",
			mutate: func(t *testing.T, path string) {
				if err := os.Link(path, path+".alias"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			mutate: func(t *testing.T, path string) {
				target := path + ".target"
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := deploymentFixture(t)
			if _, err := manager.Init(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(manager.StateDir, "deployment", "active.json")
			test.mutate(t, path)
			if _, err := ReadActive(manager.StateDir); !IsKind(err, ErrorInvalidState) {
				t.Fatalf("unsafe active error = %v", err)
			}
		})
	}

	manager := deploymentFixture(t)
	if _, err := manager.Init(); err != nil {
		t.Fatal(err)
	}
	other := Identity{ProjectCommit: strings.Repeat("f", 40), Release: "RELEASE.2026-08-28T13-34-56Z-gffffffffffff"}
	if _, err := RequireActive(manager.StateDir, other); !IsKind(err, ErrorConflict) {
		t.Fatalf("mismatched active error = %v", err)
	}
}

func TestConcurrentInitPublishesOneIdentity(t *testing.T) {
	manager := deploymentFixture(t)
	manager.Now = func() time.Time { return testNow }
	statuses := make(chan string, 2)
	errorsFound := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := manager.Init()
			if err != nil {
				errorsFound <- err
				return
			}
			statuses <- result.Status
		}()
	}
	wait.Wait()
	close(statuses)
	close(errorsFound)
	for err := range errorsFound {
		t.Errorf("concurrent initialize: %v", err)
	}
	counts := map[string]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[StatusInitialized] != 1 || counts[StatusAlreadyInitialized] != 1 {
		t.Fatalf("concurrent statuses = %#v", counts)
	}
}

func TestActivateTransitionsOnlyExpectedIdentityAndIsIdempotent(t *testing.T) {
	manager := deploymentFixture(t)
	if _, err := manager.Init(); err != nil {
		t.Fatal(err)
	}
	expected := Identity{ProjectCommit: testCommit, Release: testRelease}
	targetCommit := strings.Repeat("f", 40)
	targetRelease := "RELEASE.2026-08-28T13-34-56Z-gffffffffffff"
	manager.ProjectCommit = targetCommit
	manager.Release = targetRelease
	manager.Now = func() time.Time { return testNow.Add(time.Hour) }

	active, err := manager.Activate(expected)
	if err != nil || active.Identity() != (Identity{ProjectCommit: targetCommit, Release: targetRelease}) ||
		active.ActivatedAt != "2026-08-28T13:34:56.789Z" {
		t.Fatalf("activate = %#v/%v", active, err)
	}
	bytesAfterTransition, err := os.ReadFile(filepath.Join(manager.StateDir, "deployment", "active.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager.Now = func() time.Time { return testNow.Add(2 * time.Hour) }
	repeated, err := manager.Activate(expected)
	if err != nil || repeated != active {
		t.Fatalf("repeat activate = %#v/%v", repeated, err)
	}
	bytesAfterRepeat, err := os.ReadFile(filepath.Join(manager.StateDir, "deployment", "active.json"))
	if err != nil || string(bytesAfterRepeat) != string(bytesAfterTransition) {
		t.Fatalf("repeat rewrote active = %q/%v", bytesAfterRepeat, err)
	}

	conflicting := manager
	conflicting.ProjectCommit = strings.Repeat("e", 40)
	conflicting.Release = "RELEASE.2026-08-28T14-34-56Z-geeeeeeeeeeee"
	if _, err := conflicting.Activate(expected); !IsKind(err, ErrorConflict) {
		t.Fatalf("unexpected transition source error = %v", err)
	}
}

func TestActivateAtOrAfterReaffirmsSameIdentityOnce(t *testing.T) {
	manager := deploymentFixture(t)
	if _, err := manager.Init(); err != nil {
		t.Fatal(err)
	}
	identity := Identity{ProjectCommit: testCommit, Release: testRelease}
	minimum := testNow.Add(time.Hour)
	manager.Now = func() time.Time { return minimum }
	active, err := manager.ActivateAtOrAfter(identity, minimum)
	if err != nil || active.Identity() != identity || active.ActivatedAt != "2026-08-28T13:34:56.789Z" {
		t.Fatalf("reaffirm = %#v/%v", active, err)
	}
	before, err := os.ReadFile(filepath.Join(manager.StateDir, "deployment", "active.json"))
	if err != nil {
		t.Fatal(err)
	}
	manager.Now = func() time.Time { return minimum.Add(time.Hour) }
	repeated, err := manager.ActivateAtOrAfter(identity, minimum)
	if err != nil || repeated != active {
		t.Fatalf("repeat reaffirm = %#v/%v", repeated, err)
	}
	after, err := os.ReadFile(filepath.Join(manager.StateDir, "deployment", "active.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("repeat reaffirm rewrote active = %q/%v", after, err)
	}
	if _, err := manager.ActivateAtOrAfter(identity, time.Time{}); !IsKind(err, ErrorInvalidState) {
		t.Fatalf("zero minimum error = %v", err)
	}
}

func deploymentFixture(t *testing.T) Manager {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	dataDir := filepath.Join(root, "data")
	for _, path := range []string{stateDir, dataDir} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return Manager{
		StateDir: stateDir, DataDir: dataDir,
		ProjectCommit: testCommit, Release: testRelease,
		Now: func() time.Time { return testNow },
	}
}

func writeActiveFixture(t *testing.T, stateDir string, contents []byte) {
	t.Helper()
	directory := filepath.Join(stateDir, "deployment")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "active.json"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityValidation(t *testing.T) {
	for _, identity := range []Identity{
		{},
		{ProjectCommit: "short", Release: testRelease},
		{ProjectCommit: strings.ToUpper(testCommit), Release: testRelease},
		{ProjectCommit: testCommit, Release: "development"},
		{ProjectCommit: testCommit, Release: fmt.Sprintf("RELEASE.2026-08-28T12-34-56Z-g%s", strings.Repeat("f", 12))},
	} {
		if err := identity.Validate(); err == nil {
			t.Fatalf("invalid identity accepted: %#v", identity)
		}
	}
}
