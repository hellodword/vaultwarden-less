package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/deployment"
)

const (
	gateCommit  = "0123456789abcdef0123456789abcdef01234567"
	gateRelease = "RELEASE.2026-08-28T12-34-56Z-g0123456789ab"
)

func TestGateRejectsUsageAndDirtyBuild(t *testing.T) {
	for _, arguments := range [][]string{nil, {modeNormal, "extra"}, {"unknown"}} {
		var stderr bytes.Buffer
		started := false
		environment := gateTestEnvironment()
		environment.start = func() error { started = true; return nil }
		if code := execute(context.Background(), arguments, &stderr, environment); code != 2 || started || stderr.Len() == 0 {
			t.Fatalf("usage %#v = %d/%q/started=%t", arguments, code, stderr.String(), started)
		}
	}

	environment := gateTestEnvironment()
	environment.ready = func() bool { return false }
	started := false
	environment.start = func() error { started = true; return nil }
	var stderr bytes.Buffer
	if code := execute(context.Background(), []string{modeNormal}, &stderr, environment); code != 1 || started {
		t.Fatalf("dirty gate = %d/%q/started=%t", code, stderr.String(), started)
	}

}

func TestAllGatesRunBeforeExec(t *testing.T) {
	for _, mode := range []string{modeNormal, modeUpgradeCandidate, modeRollback, modeDrill, modeImport} {
		t.Run(mode, func(t *testing.T) {
			environment := gateTestEnvironment()
			gated, started := false, false
			switch mode {
			case modeNormal:
				environment.allowNormal = func(_ context.Context, _, dataDir string, identity deployment.Identity, _ config.Deployment) error {
					gated = dataDir == "/data" && identity.ProjectCommit == gateCommit && identity.Release == gateRelease
					return nil
				}
			case modeUpgradeCandidate:
				environment.allowCandidate = func(_, commit, release, activeCommit, activeRelease string, _ config.Deployment) error {
					gated = commit == gateCommit && release == gateRelease && activeCommit == gateCommit && activeRelease == gateRelease
					return nil
				}
			case modeDrill:
				environment.allowDrill = func(_, commit, release, contentID, archiveID string, _ config.Deployment) error {
					gated = commit == gateCommit && release == gateRelease && validDigest(contentID) && validDigest(archiveID)
					return nil
				}
			case modeRollback:
				environment.allowRollback = func(_ context.Context, _, dataDir string, identity deployment.Identity, _ config.Deployment) error {
					gated = dataDir == "/data" && identity.ProjectCommit == gateCommit && identity.Release == gateRelease
					return nil
				}
			case modeImport:
				environment.allowImport = func(_ context.Context, _, sourceDir, candidateDir string, identity deployment.Identity, _ config.Deployment) error {
					gated = sourceDir == "/import/source" && candidateDir == "/data" && identity.ProjectCommit == gateCommit && identity.Release == gateRelease
					return nil
				}
			}
			environment.start = func() error { started = true; return nil }
			var stderr bytes.Buffer
			if code := execute(context.Background(), []string{mode}, &stderr, environment); code != 0 || stderr.Len() != 0 || !gated || !started {
				t.Fatalf("gate %s = %d/%q/gated=%t/started=%t", mode, code, stderr.String(), gated, started)
			}
		})
	}
}

func TestGateFailureNeverStartsVaultwarden(t *testing.T) {
	for _, mode := range []string{modeNormal, modeUpgradeCandidate, modeRollback, modeDrill, modeImport} {
		t.Run(mode, func(t *testing.T) {
			environment := gateTestEnvironment()
			sentinel := errors.New("synthetic gate failure")
			switch mode {
			case modeNormal:
				environment.requireActive = func(string, deployment.Identity) (deployment.Active, error) {
					return deployment.Active{}, sentinel
				}
			case modeUpgradeCandidate:
				environment.readActive = func(string) (deployment.Active, error) { return deployment.Active{}, sentinel }
			case modeDrill:
				environment.readDrillManifest = func(context.Context, string) (drillManifest, error) {
					return drillManifest{}, sentinel
				}
			case modeRollback:
				environment.allowRollback = func(context.Context, string, string, deployment.Identity, config.Deployment) error {
					return sentinel
				}
			case modeImport:
				environment.allowImport = func(context.Context, string, string, string, deployment.Identity, config.Deployment) error {
					return sentinel
				}
			}
			started := false
			environment.start = func() error { started = true; return nil }
			var stderr bytes.Buffer
			if code := execute(context.Background(), []string{mode}, &stderr, environment); code != 1 || started || strings.Contains(stderr.String(), sentinel.Error()) {
				t.Fatalf("failed gate %s = %d/%q/started=%t", mode, code, stderr.String(), started)
			}
		})
	}
}

func TestReadDrillManifestIsStrictAndPrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "recovery")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"project_commit":"` + gateCommit + `","content_id":"sha256:` + strings.Repeat("a", 64) + `","archive_id":"sha256:` + strings.Repeat("b", 64) + `","files":[{"path":"db.sqlite3"}]}` + "\n"
	path := filepath.Join(root, "manifest.json")
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	decoded, err := readDrillManifest(context.Background(), root)
	if err != nil || decoded.ProjectCommit != gateCommit {
		t.Fatalf("read manifest = %#v/%v", decoded, err)
	}
	for name, contents := range map[string]string{
		"unknown":  strings.Replace(manifest, `,"files"`, `,"unknown":true,"files"`, 1),
		"trailing": manifest + "{}\n",
		"mismatch": strings.Replace(manifest, gateCommit, strings.Repeat("f", 39)+"z", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readDrillManifest(context.Background(), root); err == nil {
				t.Fatal("invalid manifest was accepted")
			}
		})
	}
	if err := os.WriteFile(path, []byte(manifest), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := readDrillManifest(context.Background(), root); err == nil {
		t.Fatal("non-private manifest was accepted")
	}
}

func gateTestEnvironment() gateEnvironment {
	identity := deployment.Identity{ProjectCommit: gateCommit, Release: gateRelease}
	return gateEnvironment{
		ready:    func() bool { return true },
		identity: func() deployment.Identity { return identity },
		loadDeployment: func(string) (config.Deployment, error) {
			return config.Deployment{Root: config.DeploymentRoot, RuntimeUID: uint32(os.Geteuid()), RuntimeGID: uint32(os.Getegid()), ListenPort: 8080, DrillPort: 8081}, nil
		},
		requireActive: func(string, deployment.Identity) (deployment.Active, error) {
			return deployment.Active{ProjectCommit: gateCommit, Release: gateRelease}, nil
		},
		readActive: func(string) (deployment.Active, error) {
			return deployment.Active{ProjectCommit: gateCommit, Release: gateRelease}, nil
		},
		allowNormal:    func(context.Context, string, string, deployment.Identity, config.Deployment) error { return nil },
		allowCandidate: func(string, string, string, string, string, config.Deployment) error { return nil },
		readDrillManifest: func(context.Context, string) (drillManifest, error) {
			return drillManifest{
				ProjectCommit: gateCommit,
				ContentID:     "sha256:" + strings.Repeat("a", 64), ArchiveID: "sha256:" + strings.Repeat("b", 64),
				Files: []json.RawMessage{json.RawMessage(`{"path":"db.sqlite3"}`)},
			}, nil
		},
		allowDrill:    func(string, string, string, string, string, config.Deployment) error { return nil },
		allowRollback: func(context.Context, string, string, deployment.Identity, config.Deployment) error { return nil },
		allowImport: func(context.Context, string, string, string, deployment.Identity, config.Deployment) error {
			return nil
		},
		start:           func() error { return nil },
		deploymentPath:  "/deployment/.env",
		stateDir:        "/state",
		dataDir:         "/data",
		importSourceDir: "/import/source",
		recoveryRoot:    "/recovery",
	}
}
