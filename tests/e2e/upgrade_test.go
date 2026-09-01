package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	productdeployment "github.com/hellodword/vaultwarden-less/internal/deployment"
	productsnapshot "github.com/hellodword/vaultwarden-less/internal/snapshot"
	productupgrade "github.com/hellodword/vaultwarden-less/internal/upgrade"
)

type upgradeRelease struct {
	commit             string
	release            string
	vaultwardenVersion string
	resticVersion      string
}

var resticReleaseVersionPattern = regexp.MustCompile(`^[0-9]+[.][0-9]+[.][0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)

type upgradeFixture struct {
	t              *testing.T
	ctx            context.Context
	harness        *e2eHarness
	from           upgradeRelease
	to             upgradeRelease
	upgradeID      string
	email          string
	itemID         string
	client         *bitwardenCLI
	originalRSA    [sha256.Size]byte
	ticket         []byte
	rollbackTreeID string
	markerName     string
}

func TestUpgradeAcceptLifecycle(t *testing.T) {
	if !*runUpgradeAcceptE2E {
		t.Skip("pass -run-upgrade-accept-e2e with two full clean commits to run the real accept lifecycle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	from, to := prepareUpgradePair(t, ctx, true)
	fixture := newUpgradeFixture(t, ctx, from, to)
	fixture.startCandidateAndRecordMigration()
	fixture.verifyCandidate()
	fixture.startVerifiedDrill()
	fixture.acceptAfterDecisionInterruption()
}

func TestUpgradeRollbackLifecycle(t *testing.T) {
	if !*runUpgradeRollbackE2E {
		t.Skip("pass -run-upgrade-rollback-e2e with two full clean commits to run the real deepest rollback lifecycle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	from, to := prepareUpgradePair(t, ctx, false)
	fixture := newUpgradeFixture(t, ctx, from, to)
	fixture.startCandidateAndRecordMigration()
	fixture.verifyCandidate()
	fixture.forceFinalDrillHealthFailure()
	fixture.rollbackAfterExchangeInterruption()
}

func prepareUpgradePair(
	t *testing.T,
	ctx context.Context,
	verifyRestic bool,
) (upgradeRelease, upgradeRelease) {
	t.Helper()
	imageHarness := newE2EHarnessWithImages(t, ctx, false)
	from := resolveUpgradeRelease(t, imageHarness, *upgradeFromCommit)
	to := resolveUpgradeRelease(t, imageHarness, *upgradeToCommit)
	if from.commit == to.commit {
		t.Fatal("upgrade A and B commits must differ")
	}

	prepareUpgradeImages(t, ctx, imageHarness, &from, &to)
	if from.vaultwardenVersion == to.vaultwardenVersion {
		t.Logf("upgrade framework uses one real Vaultwarden version %q; distinct-version upgrade is not verified", from.vaultwardenVersion)
	} else {
		t.Logf("upgrade framework uses distinct real Vaultwarden versions A=%q B=%q", from.vaultwardenVersion, to.vaultwardenVersion)
	}
	if resticCrossVersionRequired(from, to) {
		t.Logf("release pair uses distinct real Restic versions A=%q B=%q", from.resticVersion, to.resticVersion)
	} else {
		t.Logf("release pair uses one real Restic version %q; cross-version Restic matrix is not applicable", from.resticVersion)
	}
	if verifyRestic {
		verifyResticReleaseCompatibility(t, imageHarness, from, to)
	}
	return from, to
}

func resolveUpgradeRelease(t *testing.T, harness *e2eHarness, commit string) upgradeRelease {
	t.Helper()
	if !lowerHex40Pattern.MatchString(commit) {
		t.Fatal("upgrade commit must be full lowercase hexadecimal")
	}
	repository := repositoryRoot(t)
	gitPath, err := exec.LookPath("git")
	if err != nil {
		if !*preloadedImages {
			t.Fatalf("Git is required for upgrade identity resolution: %v", err)
		}
		return resolvePreloadedUpgradeRelease(t, harness, commit)
	}
	if output, err := runCommand(context.Background(), repository, nil, gitPath, "cat-file", "-e", commit+"^{commit}"); err != nil {
		t.Fatalf("upgrade commit is unavailable: %v: %s", err, harnessDiagnostics(output))
	}
	secondsText, err := runCommand(context.Background(), repository, nil, gitPath, "show", "-s", "--format=%ct", commit)
	if err != nil {
		t.Fatalf("read upgrade commit timestamp: %v: %s", err, harnessDiagnostics(secondsText))
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(secondsText), 10, 64)
	if err != nil || seconds <= 0 {
		t.Fatalf("upgrade commit timestamp is invalid: %q", secondsText)
	}
	instant := time.Unix(seconds, 0).UTC()
	return upgradeRelease{
		commit:  commit,
		release: "RELEASE." + instant.Format("2006-01-02T15-04-05Z") + "-g" + commit[:12],
	}
}

func resolvePreloadedUpgradeRelease(t *testing.T, harness *e2eHarness, commit string) upgradeRelease {
	t.Helper()
	output, err := harness.docker(
		"image", "ls", "--no-trunc", "--format", "{{.Repository}}:{{.Tag}}",
	)
	if err != nil {
		t.Fatalf("list preloaded upgrade images: %v: %s", err, harnessDiagnostics(output))
	}
	releases := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		reference := strings.TrimSpace(line)
		const prefix = "vaultwarden-less-backup:"
		if !strings.HasPrefix(reference, prefix) {
			continue
		}
		release := strings.TrimPrefix(reference, prefix)
		if !productupgrade.ValidRelease(release, commit) {
			continue
		}
		inspect, inspectError := harness.docker("image", "inspect", reference)
		if inspectError != nil {
			continue
		}
		var documents []struct {
			Config struct {
				Labels map[string]string `json:"Labels"`
			} `json:"Config"`
		}
		if json.Unmarshal([]byte(inspect), &documents) == nil && len(documents) == 1 &&
			documents[0].Config.Labels["org.opencontainers.image.revision"] == commit &&
			documents[0].Config.Labels["org.opencontainers.image.version"] == release {
			releases[release] = true
		}
	}
	if len(releases) != 1 {
		t.Fatal("preloaded upgrade commit does not have one unambiguous immutable release")
	}
	for release := range releases {
		return upgradeRelease{commit: commit, release: release}
	}
	return upgradeRelease{}
}

func prepareUpgradeImages(
	t *testing.T,
	ctx context.Context,
	harness *e2eHarness,
	from, to *upgradeRelease,
) {
	t.Helper()
	if !*preloadedImages {
		gitPath, err := exec.LookPath("git")
		if err != nil {
			t.Fatalf("Git is required to build exact upgrade releases: %v", err)
		}
		nixPath, err := exec.LookPath("nix")
		if err != nil {
			t.Fatalf("Nix is required to build exact upgrade releases: %v", err)
		}
		for index, release := range []*upgradeRelease{from, to} {
			clone := filepath.Join(harness.localRoot, fmt.Sprintf("upgrade-source-%d", index))
			if output, err := runCommand(ctx, harness.localRoot, nil, gitPath, "clone", "--quiet", "--no-hardlinks", harness.repository, clone); err != nil {
				t.Fatalf("clone exact upgrade source: %v: %s", err, harnessDiagnostics(output))
			}
			if output, err := runCommand(ctx, clone, nil, gitPath, "checkout", "--quiet", "--detach", release.commit); err != nil {
				t.Fatalf("checkout exact upgrade source: %v: %s", err, harnessDiagnostics(output))
			}
			outputRoot := filepath.Join(harness.localRoot, fmt.Sprintf("upgrade-release-%d", index))
			if output, err := runCommandWithEnvironment(
				ctx, clone, nil, harness.dockerEnv, nixPath,
				"run", ".#release", "--", "--output-root", outputRoot,
			); err != nil {
				t.Fatalf("build exact upgrade release: %v: %s", err, harnessDiagnostics(output))
			}
			if output, err := runCommandWithEnvironment(
				ctx, clone, nil, harness.dockerEnv, nixPath,
				"run", ".#image-load", "--", outputRoot,
			); err != nil {
				t.Fatalf("load exact upgrade release: %v: %s", err, harnessDiagnostics(output))
			}
		}
		output, err := runCommandWithEnvironment(
			ctx, harness.repository, nil, harness.dockerEnv, nixPath,
			"build", "--no-link", "--print-out-paths", ".#moto-image",
		)
		if err != nil {
			t.Fatalf("build upgrade Moto image: %v: %s", err, harnessDiagnostics(output))
		}
		paths := strings.Fields(output)
		if len(paths) != 1 {
			t.Fatalf("Nix returned invalid Moto image output: %q", output)
		}
		if loaded, err := harness.docker("load", "--input", paths[0]); err != nil {
			t.Fatalf("load upgrade Moto image: %v: %s", err, harnessDiagnostics(loaded))
		}
	}
	for _, release := range []*upgradeRelease{from, to} {
		validateUpgradeReleaseImages(t, harness, release)
		release.vaultwardenVersion = vaultwardenImageVersion(t, harness, *release)
		release.resticVersion = resticImageVersion(t, harness, *release)
	}
	if output, err := harness.docker("image", "inspect", "vaultwarden-less-moto:local"); err != nil {
		t.Fatalf("preloaded Moto image is unavailable: %v: %s", err, harnessDiagnostics(output))
	}
}

func validateUpgradeReleaseImages(t *testing.T, harness *e2eHarness, release *upgradeRelease) {
	t.Helper()
	for _, product := range []string{"backup", "proxy", "vaultwarden"} {
		reference := "vaultwarden-less-" + product + ":" + release.release
		output, err := harness.docker("image", "inspect", reference)
		if err != nil {
			t.Fatalf("immutable upgrade image is unavailable: %v: %s", err, harnessDiagnostics(output))
		}
		var documents []struct {
			Config struct {
				Labels map[string]string `json:"Labels"`
			} `json:"Config"`
		}
		if err := json.Unmarshal([]byte(output), &documents); err != nil || len(documents) != 1 {
			t.Fatalf("decode immutable image identity: %v", err)
		}
		labels := documents[0].Config.Labels
		if labels["org.opencontainers.image.revision"] != release.commit ||
			labels["org.opencontainers.image.version"] != release.release {
			t.Fatalf("immutable image identity does not match commit %s", release.commit)
		}
	}
}

func vaultwardenImageVersion(t *testing.T, harness *e2eHarness, release upgradeRelease) string {
	t.Helper()
	output, err := harness.docker(
		"run", "--rm", "--pull", "never", "--network", "none", "--user", "1000:1000",
		"--read-only", "--security-opt", "no-new-privileges:true", "--cap-drop", "ALL",
		"--entrypoint", "/bin/vaultwarden", "vaultwarden-less-vaultwarden:"+release.release, "--version",
	)
	if err != nil || strings.TrimSpace(output) == "" {
		t.Fatalf("read immutable Vaultwarden version: %v: %s", err, harnessDiagnostics(output))
	}
	return strings.TrimSpace(output)
}

func resticImageVersion(t *testing.T, harness *e2eHarness, release upgradeRelease) string {
	t.Helper()
	output, err := harness.docker(
		"run", "--rm", "--pull", "never", "--network", "none", "--user", "1000:1000",
		"--read-only", "--security-opt", "no-new-privileges:true", "--cap-drop", "ALL",
		"--entrypoint", "/bin/restic", "vaultwarden-less-backup:"+release.release, "version", "--json",
	)
	if err != nil {
		t.Fatalf("read immutable Restic version: %v: %s", err, harnessDiagnostics(output))
	}
	var document struct {
		MessageType string `json:"message_type"`
		Version     string `json:"version"`
	}
	if err := json.Unmarshal([]byte(output), &document); err != nil || document.MessageType != "version" ||
		!resticReleaseVersionPattern.MatchString(document.Version) {
		t.Fatalf("immutable Restic version output is invalid: %v", err)
	}
	return document.Version
}

func resticCrossVersionRequired(from, to upgradeRelease) bool {
	return from.resticVersion != to.resticVersion
}

type resticReleaseMount struct {
	source   string
	target   string
	readOnly bool
}

func verifyResticReleaseCompatibility(t *testing.T, harness *e2eHarness, from, to upgradeRelease) {
	t.Helper()
	if !resticCrossVersionRequired(from, to) {
		return
	}

	root := filepath.Join(harness.localRoot, "restic-release-compatibility")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory := func(name string) string {
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	repository := directory("repository")
	cacheA := directory("cache-a")
	cacheB := directory("cache-b")
	dataA := directory("data-a")
	dataB := directory("data-b")
	restoreBA := directory("restore-b-from-a")
	restoreAB := directory("restore-a-from-b")
	password := filepath.Join(root, "password")
	writePrivateFile(t, password, "synthetic-restic-release-password\n")
	writePrivateFile(t, filepath.Join(dataA, "a.txt"), "written by release A\n")
	writePrivateFile(t, filepath.Join(dataB, "a.txt"), "written by release A\n")
	writePrivateFile(t, filepath.Join(dataB, "b.txt"), "written by release B\n")

	containerName := harness.project + "-restic-release"
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(harness.ctx), 30*time.Second)
		defer cancel()
		_, _ = harness.dockerContext(cleanupContext, "rm", "--force", containerName)
		if _, err := harness.dockerContext(cleanupContext, "container", "inspect", containerName); err == nil {
			t.Error("Restic release compatibility container survived exact cleanup")
		}
	})
	run := func(release upgradeRelease, cache string, mounts []resticReleaseMount, arguments ...string) string {
		t.Helper()
		uid := strconv.Itoa(os.Geteuid())
		gid := strconv.Itoa(os.Getegid())
		dockerArguments := []string{
			"run", "--rm", "--pull", "never", "--name", containerName,
			"--label", "vaultwarden-less.e2e.project=" + harness.project,
			"--network", "none", "--user", uid + ":" + gid, "--read-only",
			"--security-opt", "no-new-privileges:true", "--cap-drop", "ALL",
			"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=16m,mode=0700,uid=" + uid + ",gid=" + gid,
			"--env", "HOME=/nonexistent", "--env", "TMPDIR=/tmp",
			"--mount", "type=bind,src=" + repository + ",dst=/repository",
			"--mount", "type=bind,src=" + password + ",dst=/credentials/password,readonly",
			"--mount", "type=bind,src=" + cache + ",dst=/cache",
		}
		for _, mount := range mounts {
			value := "type=bind,src=" + mount.source + ",dst=" + mount.target
			if mount.readOnly {
				value += ",readonly"
			}
			dockerArguments = append(dockerArguments, "--mount", value)
		}
		dockerArguments = append(
			dockerArguments,
			"--entrypoint", "/bin/restic", "vaultwarden-less-backup:"+release.release,
			"--repo", "/repository", "--password-file", "/credentials/password", "--cache-dir", "/cache",
		)
		dockerArguments = append(dockerArguments, arguments...)
		output, err := harness.docker(dockerArguments...)
		if err != nil {
			t.Fatalf("Restic %s command failed: %v: %s", arguments[0], err, harnessDiagnostics(output))
		}
		return output
	}

	run(from, cacheA, nil, "init", "--repository-version", "2", "--json")
	var repositoryConfig struct {
		Version int `json:"version"`
	}
	configOutput := run(from, cacheA, nil, "cat", "config")
	if err := json.Unmarshal([]byte(configOutput), &repositoryConfig); err != nil || repositoryConfig.Version != 2 {
		t.Fatalf("release A repository config = %#v/%v, want version 2", repositoryConfig, err)
	}
	aSnapshot := resticReleaseBackupID(t, run(
		from, cacheA,
		[]resticReleaseMount{{source: dataA, target: "/source", readOnly: true}},
		"backup", "--json", "--host", "vaultwarden-less-release-compatibility", "--tag", "writer=a", "--", "/source",
	))
	assertResticReleaseSnapshots(t, run(to, cacheB, nil, "snapshots", "--json"), aSnapshot)
	run(to, cacheB, nil, "check", "--json")
	run(
		to, cacheB,
		[]resticReleaseMount{{source: restoreBA, target: "/restore"}},
		"restore", "--json", "--verify", "--target", "/restore", aSnapshot,
	)
	assertResticReleaseRestore(t, restoreBA, map[string]string{"source/a.txt": "written by release A\n"})

	bSnapshot := resticReleaseBackupID(t, run(
		to, cacheB,
		[]resticReleaseMount{{source: dataB, target: "/source", readOnly: true}},
		"backup", "--json", "--host", "vaultwarden-less-release-compatibility", "--tag", "writer=b", "--", "/source",
	))
	if bSnapshot == aSnapshot {
		t.Fatal("release B did not create a distinct snapshot")
	}
	assertResticReleaseSnapshots(t, run(from, cacheA, nil, "snapshots", "--json"), aSnapshot, bSnapshot)
	run(from, cacheA, nil, "check", "--json")
	run(
		from, cacheA,
		[]resticReleaseMount{{source: restoreAB, target: "/restore"}},
		"restore", "--json", "--verify", "--target", "/restore", bSnapshot,
	)
	assertResticReleaseRestore(t, restoreAB, map[string]string{
		"source/a.txt": "written by release A\n",
		"source/b.txt": "written by release B\n",
	})
}

func resticReleaseBackupID(t *testing.T, output string) string {
	t.Helper()
	result := ""
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var message struct {
			MessageType string `json:"message_type"`
			SnapshotID  string `json:"snapshot_id"`
		}
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Fatalf("decode Restic backup output: %v", err)
		}
		if message.MessageType == "summary" {
			if result != "" || !lowerHex64Pattern.MatchString(message.SnapshotID) {
				t.Fatal("Restic backup returned an invalid summary")
			}
			result = message.SnapshotID
		}
	}
	if result == "" {
		t.Fatal("Restic backup returned no summary")
	}
	return result
}

func assertResticReleaseSnapshots(t *testing.T, output string, expected ...string) {
	t.Helper()
	var documents []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(output), &documents); err != nil {
		t.Fatalf("decode Restic snapshots: %v", err)
	}
	actual := make(map[string]bool, len(documents))
	for _, document := range documents {
		if !lowerHex64Pattern.MatchString(document.ID) || actual[document.ID] {
			t.Fatal("Restic snapshots returned an invalid identity set")
		}
		actual[document.ID] = true
	}
	if len(actual) != len(expected) {
		t.Fatalf("Restic snapshots = %#v, want %v", actual, expected)
	}
	for _, id := range expected {
		if !actual[id] {
			t.Fatalf("Restic snapshots omitted %s", id)
		}
	}
}

func assertResticReleaseRestore(t *testing.T, root string, expected map[string]string) {
	t.Helper()
	actual := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkError error) error {
		if walkError != nil {
			return walkError
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeType != 0 {
			return fmt.Errorf("restored special entry %s", entry.Name())
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		actual[filepath.ToSlash(relative)] = string(content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(actual, expected) {
		t.Fatalf("restored files = %#v, want %#v", actual, expected)
	}
}

func TestResticCrossVersionDecisionAndOutputParsers(t *testing.T) {
	from := upgradeRelease{resticVersion: "0.19.1"}
	to := upgradeRelease{resticVersion: "0.19.1"}
	if resticCrossVersionRequired(from, to) {
		t.Fatal("equal actual Restic versions selected a cross-version matrix")
	}
	to.resticVersion = "0.20.0"
	if !resticCrossVersionRequired(from, to) {
		t.Fatal("distinct actual Restic versions did not select a cross-version matrix")
	}
	first := strings.Repeat("a", 64)
	second := strings.Repeat("b", 64)
	backup := `{"message_type":"status"}` + "\n" + `{"message_type":"summary","snapshot_id":"` + first + `"}`
	if got := resticReleaseBackupID(t, backup); got != first {
		t.Fatalf("backup ID = %s", got)
	}
	snapshots, err := json.Marshal([]map[string]string{{"id": first}, {"id": second}})
	if err != nil {
		t.Fatal(err)
	}
	assertResticReleaseSnapshots(t, string(snapshots), first, second)
}

func (h *e2eHarness) selectProductionRelease(release upgradeRelease) {
	h.t.Helper()
	for _, product := range []string{"backup", "proxy", "vaultwarden"} {
		immutable := "vaultwarden-less-" + product + ":" + release.release
		local := "vaultwarden-less-" + product + ":local"
		if output, err := h.docker("image", "tag", immutable, local); err != nil {
			h.t.Fatalf("select immutable production release: %v: %s", err, harnessDiagnostics(output))
		}
	}
}

func newUpgradeFixture(
	t *testing.T,
	ctx context.Context,
	from, to upgradeRelease,
) *upgradeFixture {
	t.Helper()
	harness := newE2EHarnessWithImages(t, ctx, false)
	harness.setDockerEnvironment("A_RELEASE", from.release)
	harness.setDockerEnvironment("B_RELEASE", to.release)
	harness.validateUpgradeRollbackComposeModel()
	harness.selectProductionRelease(from)
	harness.startMotoAndRepository()
	harness.initializeDeployment()
	harness.startVaultwarden()

	unique := time.Now().UnixNano()
	email := fmt.Sprintf("vaultwarden-less-e2e-upgrade-%d@example.com", unique)
	_ = registerAccount(ctx, t, harness.publicHTTP, email, testPassword)
	client := newBitwardenCLI(t, filepath.Join(harness.localRoot, "bw-upgrade-a"), harness.publicTLS, harness.tlsCAFile, email)
	client.login(ctx)
	itemID := client.createItem(ctx, "Upgrade Recovery Item", initialItemPassword, "")
	harness.startBackup()
	_ = harness.waitSnapshotCount(1, 5*time.Minute)
	harness.startProxy()

	rsaPath := filepath.Join(harness.localRoot, "data", "rsa_key.pem")
	rsaBytes, err := os.ReadFile(rsaPath)
	if err != nil || len(rsaBytes) == 0 {
		t.Fatalf("read A RSA identity: %v", err)
	}
	prepare := runUpgradeResult(t, harness.compose(
		"--profile", "maintenance", "run", "--rm", "--no-deps", "maintenance",
		"upgrade", "prepare", "--to-commit", to.commit, "--to-release", to.release,
	), productupgrade.PhasePrepared, from, to)

	composeEnvironment := filepath.Join(harness.localRoot, "state", "upgrade", "current", "compose.env")
	validateUpgradeComposeEnvironment(t, composeEnvironment, from, to)
	harness.stopProduction()
	harness.assertProxyClosed()
	freezeCommand := []string{"--profile", "upgrade", "run", "--rm", "--no-deps", "upgrade-a", "upgrade", "freeze"}
	freezeOutput, freezeError := harness.tryUpgradeCompose(harness.ctx, true, freezeCommand...)
	if freezeError != nil {
		t.Fatalf(
			"freeze failed: %v: %s; source metadata: %s",
			freezeError,
			harnessDiagnostics(freezeOutput),
			upgradeSourceMetadata(harness.localRoot),
		)
	}
	freeze := runUpgradeResult(t, freezeOutput, productupgrade.PhaseFrozen, from, to)
	if len(freeze.Targets) != 1 || freeze.Targets[0].Target != "primary" {
		t.Fatalf("freeze evidence is incomplete: %#v", freeze.Targets)
	}
	ticketPath := filepath.Join(harness.localRoot, "state", "upgrade", "current", "rollback-ticket.json")
	ticket, err := os.ReadFile(ticketPath)
	if err != nil || len(ticket) == 0 {
		t.Fatalf("read immutable A rollback ticket: %v", err)
	}
	var ticketDocument productupgrade.RollbackTicket
	if err := decodeExactJSON(string(ticket), &ticketDocument); err != nil || ticketDocument.Phase != productupgrade.PhaseFrozen ||
		ticketDocument.UpgradeID != prepare.UpgradeID || ticketDocument.ToCommit != to.commit {
		t.Fatalf("rollback ticket is invalid: %v", err)
	}
	markerName := "e2e-b-only-marker"
	writePrivateFile(t, filepath.Join(harness.localRoot, "data", markerName), "candidate-only\n")
	harness.selectProductionRelease(to)

	return &upgradeFixture{
		t: t, ctx: ctx, harness: harness, from: from, to: to,
		upgradeID: prepare.UpgradeID, email: email, itemID: itemID, client: client,
		originalRSA: sha256.Sum256(rsaBytes), ticket: ticket, rollbackTreeID: ticketDocument.RollbackTreeID,
		markerName: markerName,
	}
}

func upgradeSourceMetadata(root string) string {
	data := filepath.Join(root, "data")
	entries := []string{}
	_ = filepath.WalkDir(data, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			entries = append(entries, "walk-error")
			return filepath.SkipAll
		}
		if len(entries) >= 32 {
			return filepath.SkipAll
		}
		info, err := os.Lstat(path)
		if err != nil {
			entries = append(entries, "stat-error")
			return nil
		}
		relative, err := filepath.Rel(data, path)
		if err != nil {
			relative = "relative-error"
		}
		entries = append(entries, fmt.Sprintf("%s:%s:%04o", filepath.ToSlash(relative), info.Mode().Type(), info.Mode().Perm()))
		return nil
	})
	slices.Sort(entries)
	return strings.Join(entries, ",")
}

func validateUpgradeComposeEnvironment(t *testing.T, path string, from, to upgradeRelease) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("upgrade compose environment is not a private regular file: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 7 || lines[5] != "A_RELEASE="+from.release || lines[6] != "B_RELEASE="+to.release {
		t.Fatalf("upgrade compose environment has an invalid exact shape")
	}
}

func readPrivateText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (h *e2eHarness) tryUpgradeCompose(ctx context.Context, generatedEnvironment bool, arguments ...string) (string, error) {
	environment := filepath.Join(h.localRoot, ".env")
	if generatedEnvironment {
		environment = filepath.Join(h.localRoot, "state", "upgrade", "current", "compose.env")
	}
	base := []string{
		"compose", "--env-file", environment, "--project-directory", h.localRoot,
		"--project-name", h.project, "--file", h.productionCompose, "--file", h.upgradeCompose,
		"--file", h.testCompose, "--file", h.upgradeTestCompose,
	}
	return h.dockerContext(ctx, append(base, arguments...)...)
}

func (h *e2eHarness) upgradeComposeCommand(generatedEnvironment bool, arguments ...string) string {
	h.t.Helper()
	output, err := h.tryUpgradeCompose(h.ctx, generatedEnvironment, arguments...)
	if err != nil {
		h.t.Fatalf("upgrade Compose command failed: %v: %s", err, harnessDiagnostics(output))
	}
	return output
}

func (h *e2eHarness) upgradeCommand(service string, arguments ...string) string {
	base := []string{"--profile", "upgrade", "run", "--rm", "--no-deps", service}
	output, err := h.tryUpgradeCompose(h.ctx, true, append(base, arguments...)...)
	if err != nil {
		h.t.Fatalf(
			"upgrade Compose service %s command %s failed: %v: %s",
			service,
			strings.Join(arguments, " "),
			err,
			harnessDiagnostics(output),
		)
	}
	return output
}

func (h *e2eHarness) validateUpgradeRollbackComposeModel() {
	h.t.Helper()
	output, err := h.tryUpgradeCompose(h.ctx, false, "--profile", "upgrade", "config", "--format", "json")
	if err != nil {
		h.t.Fatalf("render upgrade Compose model: %v: %s", err, harnessDiagnostics(output))
	}
	var model struct {
		Services map[string]composeGateService `json:"services"`
	}
	if err := json.Unmarshal([]byte(output), &model); err != nil {
		h.t.Fatalf("decode upgrade Compose model: %v: %s", err, harnessDiagnostics(output))
	}
	service, exists := model.Services["rollback-a"]
	if !exists {
		h.t.Fatal("rollback service is missing")
	}
	assertRollbackAdminIsolation(h.t, service, h.localRoot)
}

func runUpgradeResult(
	t *testing.T,
	output, phase string,
	from, to upgradeRelease,
) productupgrade.Result {
	t.Helper()
	var result productupgrade.Result
	if err := decodeExactJSON(output, &result); err != nil || result.Phase != phase ||
		result.FromCommit != from.commit || result.ToCommit != to.commit ||
		result.UpgradeID != from.commit+".."+to.commit || result.Targets == nil {
		t.Fatalf("invalid %s upgrade result: %v: %s", phase, err, harnessDiagnostics(output))
	}
	return result
}

func (fixture *upgradeFixture) startCandidateAndRecordMigration() {
	fixture.t.Helper()
	fixture.harness.upgradeComposeCommand(
		true,
		"--profile",
		"upgrade",
		"up",
		"--detach",
		"--no-deps",
		"upgrade-vaultwarden-b",
	)
	fixture.harness.waitComposeServiceHealthy("upgrade-vaultwarden-b", 3*time.Minute)
	fixture.harness.setBackend(fixture.harness.mappedComposeServiceEndpoint("upgrade-vaultwarden-b", 80))
	fixture.harness.waitHTTP(fixture.harness.publicHTTP+"/alive", 200, time.Minute)
	result := runUpgradeResult(
		fixture.t,
		fixture.harness.upgradeCommand("upgrade-b", "upgrade", "migrate-check"),
		productupgrade.PhaseMigrated,
		fixture.from,
		fixture.to,
	)
	if len(result.Targets) != 0 {
		fixture.t.Fatalf("migrate-check unexpectedly returned targets: %#v", result.Targets)
	}
	fixture.harness.upgradeComposeCommand(true, "--profile", "upgrade", "stop", "upgrade-vaultwarden-b")
}

func (fixture *upgradeFixture) verifyCandidate() {
	fixture.t.Helper()
	result := runUpgradeResult(
		fixture.t,
		fixture.harness.upgradeCommand("upgrade-b", "upgrade", "verify"),
		productupgrade.PhaseVerified,
		fixture.from,
		fixture.to,
	)
	if len(result.Targets) != 1 || result.Targets[0].Target != "primary" {
		fixture.t.Fatalf("candidate verification evidence is incomplete: %#v", result.Targets)
	}
}

func (fixture *upgradeFixture) forceFinalDrillHealthFailure() {
	fixture.t.Helper()
	fixture.startVerifiedDrill()
	drill := filepath.Join(fixture.harness.localRoot, "restore", "drill")
	manifestPath := filepath.Join(drill, "manifest.json")
	manifestBefore, err := os.ReadFile(manifestPath)
	if err != nil {
		fixture.t.Fatalf("read verified drill manifest: %v", err)
	}
	verifiedBefore, err := productsnapshot.Verify(fixture.ctx, drill)
	if err != nil || verifiedBefore.ProjectCommit != fixture.to.commit {
		fixture.t.Fatalf("verify retained B drill before health failure: %#v/%v", verifiedBefore, err)
	}

	fixture.harness.upgradeComposeCommand(
		true, "--profile", "drill", "stop", "drill-vaultwarden",
	)
	fixture.harness.assertProxyClosed()
	fixture.expectUpgradeFailure("verification_failure", "upgrade-b", "upgrade", "accept")
	decision := filepath.Join(fixture.harness.localRoot, "state", "upgrade", "current", "decision.json")
	if _, err := os.Lstat(decision); !os.IsNotExist(err) {
		fixture.t.Fatalf("failed drill health check created a terminal decision: %v", err)
	}
	manifestAfter, err := os.ReadFile(manifestPath)
	if err != nil || !bytes.Equal(manifestAfter, manifestBefore) {
		fixture.t.Fatalf("failed drill health check changed the retained manifest: %v", err)
	}
	verifiedAfter, err := productsnapshot.Verify(fixture.ctx, drill)
	if err != nil || verifiedAfter.ProjectCommit != fixture.to.commit ||
		verifiedAfter.ContentID != verifiedBefore.ContentID || verifiedAfter.ArchiveID != verifiedBefore.ArchiveID {
		fixture.t.Fatalf("retained B drill changed after health failure: %#v/%v", verifiedAfter, err)
	}
	_, _ = fixture.harness.tryUpgradeCompose(
		fixture.ctx, true, "--profile", "drill", "rm", "--force", "--stop", "drill-vaultwarden", "drill-copy",
	)
}

func (fixture *upgradeFixture) expectUpgradeFailure(code, service string, arguments ...string) {
	fixture.t.Helper()
	base := []string{"--profile", "upgrade", "run", "--rm", "--no-deps", service}
	output, err := fixture.harness.tryUpgradeCompose(fixture.ctx, true, append(base, arguments...)...)
	if err == nil {
		fixture.t.Fatalf("upgrade command unexpectedly succeeded: %s", harnessDiagnostics(output))
	}
	if actual := exactCommandErrorCode(fixture.t, output); actual != code {
		fixture.t.Fatalf("upgrade error code = %q, want %q", actual, code)
	}
}

func exactCommandErrorCode(t *testing.T, output string) string {
	t.Helper()
	type envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	codes := []string{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var document envelope
		if decodeExactJSON(line, &document) == nil && document.Error.Code != "" && document.Error.Message != "" {
			codes = append(codes, document.Error.Code)
		}
	}
	if len(codes) != 1 {
		t.Fatalf("failure did not contain exactly one strict command error: %s", harnessDiagnostics(output))
	}
	return codes[0]
}

func (fixture *upgradeFixture) assertFrozenFailureState() {
	fixture.t.Helper()
	activeMarker := filepath.Join(fixture.harness.localRoot, "data", fixture.markerName)
	rollbackMarker := filepath.Join(
		fixture.harness.localRoot, "state", "upgrade", "current", "rollback-data", fixture.markerName,
	)
	if value, err := os.ReadFile(activeMarker); err != nil || string(value) != "candidate-only\n" {
		fixture.t.Fatalf("candidate-only data was automatically rolled back: %q/%v", value, err)
	}
	if _, err := os.Lstat(rollbackMarker); !os.IsNotExist(err) {
		fixture.t.Fatalf("A rollback tree was modified by B: %v", err)
	}
	ticketPath := filepath.Join(fixture.harness.localRoot, "state", "upgrade", "current", "rollback-ticket.json")
	ticket, err := os.ReadFile(ticketPath)
	if err != nil || !bytes.Equal(ticket, fixture.ticket) {
		fixture.t.Fatalf("A rollback ticket changed after B failure: %v", err)
	}
	fixture.harness.assertProxyClosed()
}

func (fixture *upgradeFixture) startVerifiedDrill() {
	fixture.t.Helper()
	fixture.harness.resetUpgradeDrill()
	fixture.harness.upgradeComposeCommand(
		true,
		"--profile",
		"drill",
		"up",
		"--detach",
		"drill-vaultwarden",
	)
	fixture.harness.waitComposeServiceHealthy("drill-vaultwarden", 4*time.Minute)
	fixture.harness.setBackend(fixture.harness.mappedComposeServiceEndpoint("drill-vaultwarden", 80))
	fixture.harness.waitHTTP(fixture.harness.publicHTTP+"/alive", 200, time.Minute)
	client := newBitwardenCLI(
		fixture.t,
		filepath.Join(fixture.harness.localRoot, "bw-upgrade-drill"),
		fixture.harness.publicTLS,
		fixture.harness.tlsCAFile,
		fixture.email,
	)
	client.login(fixture.ctx)
	if itemPassword(fixture.t, client.getItem(fixture.ctx, fixture.itemID)) != initialItemPassword {
		fixture.t.Fatal("verified B drill could not read A data")
	}
}

func (fixture *upgradeFixture) assertTerminalDecision(decision string) {
	fixture.t.Helper()
	expected := fixture.to
	if decision == productupgrade.DecisionRollback {
		expected = fixture.from
	}
	active, err := productdeployment.ReadActive(filepath.Join(fixture.harness.localRoot, "state"))
	if err != nil || active.ProjectCommit != expected.commit || active.Release != expected.release {
		fixture.t.Fatalf("terminal %s active identity is invalid: %#v/%v", decision, active, err)
	}
	decisionPath := filepath.Join(
		fixture.harness.localRoot,
		"state",
		"upgrade",
		"current",
		"decision.json",
	)
	var document productupgrade.Decision
	if err := decodeExactJSON(readPrivateText(fixture.t, decisionPath), &document); err != nil ||
		document.UpgradeID != fixture.upgradeID || document.Decision != decision ||
		document.ActiveProjectCommit != expected.commit ||
		document.ActiveRelease != expected.release || document.DecidedAt == "" {
		fixture.t.Fatalf("terminal %s decision is invalid: %#v/%v", decision, document, err)
	}
}

func assertTerminalServices(t *testing.T, harness *e2eHarness, release upgradeRelease, immutable bool) {
	t.Helper()
	for index, service := range []string{"vaultwarden", "backup", "proxy"} {
		harness.waitComposeServiceHealthy(service, 4*time.Minute)
		product := []string{"vaultwarden", "backup", "proxy"}[index]
		tag := "local"
		if immutable {
			tag = release.release
		}
		reference := "vaultwarden-less-" + product + ":" + tag
		imageOutput, err := harness.docker("image", "inspect", reference)
		if err != nil {
			t.Fatalf("inspect terminal %s image: %v: %s", service, err, harnessDiagnostics(imageOutput))
		}
		var images []struct {
			ID     string `json:"Id"`
			Config struct {
				Labels map[string]string `json:"Labels"`
			} `json:"Config"`
		}
		if err := json.Unmarshal([]byte(imageOutput), &images); err != nil || len(images) != 1 ||
			images[0].Config.Labels["org.opencontainers.image.revision"] != release.commit ||
			images[0].Config.Labels["org.opencontainers.image.version"] != release.release {
			t.Fatalf("terminal %s image identity is invalid: %v", service, err)
		}
		container := harness.composeServiceContainer(service, true)
		containerOutput, err := harness.docker("container", "inspect", container)
		if err != nil {
			t.Fatalf("inspect terminal %s container: %v: %s", service, err, harnessDiagnostics(containerOutput))
		}
		var containers []struct {
			Image  string `json:"Image"`
			Config struct {
				Image string `json:"Image"`
			} `json:"Config"`
			State struct {
				Running bool `json:"Running"`
				Health  struct {
					Status string `json:"Status"`
				} `json:"Health"`
			} `json:"State"`
		}
		if err := json.Unmarshal([]byte(containerOutput), &containers); err != nil || len(containers) != 1 ||
			containers[0].Image != images[0].ID || containers[0].Config.Image != reference ||
			!containers[0].State.Running || containers[0].State.Health.Status != "healthy" {
			t.Fatalf("terminal %s container identity or health is invalid: %v", service, err)
		}
	}
}

func (fixture *upgradeFixture) assertArchived(decision string) string {
	fixture.t.Helper()
	current := filepath.Join(fixture.harness.localRoot, "state", "upgrade", "current")
	if _, err := os.Lstat(current); !os.IsNotExist(err) {
		fixture.t.Fatalf("current upgrade state remained after terminal %s: %v", decision, err)
	}
	archiveName := "history"
	oppositeName := "quarantine"
	if decision == productupgrade.DecisionRollback {
		archiveName, oppositeName = oppositeName, archiveName
	}
	archive := filepath.Join(
		fixture.harness.localRoot,
		"state",
		"upgrade",
		archiveName,
		fixture.upgradeID,
	)
	if info, err := os.Stat(archive); err != nil || !info.IsDir() {
		fixture.t.Fatalf("terminal %s archive is unavailable: %v", decision, err)
	}
	opposite := filepath.Join(
		fixture.harness.localRoot,
		"state",
		"upgrade",
		oppositeName,
		fixture.upgradeID,
	)
	if _, err := os.Lstat(opposite); !os.IsNotExist(err) {
		fixture.t.Fatalf("terminal %s created the opposite archive: %v", decision, err)
	}
	return archive
}

func (fixture *upgradeFixture) acceptAfterDecisionInterruption() {
	fixture.t.Helper()
	accepted := runUpgradeResult(
		fixture.t,
		fixture.harness.upgradeCommand("upgrade-b", "upgrade", "accept"),
		productupgrade.PhaseAccepted,
		fixture.from,
		fixture.to,
	)
	if len(accepted.Targets) != 0 {
		fixture.t.Fatalf("accept unexpectedly returned targets: %#v", accepted.Targets)
	}
	fixture.assertTerminalDecision(productupgrade.DecisionAccept)
	if _, err := os.Stat(filepath.Join(fixture.harness.localRoot, "state", "upgrade", "current")); err != nil {
		fixture.t.Fatalf("accept decision did not retain current state for resumption: %v", err)
	}
	for _, service := range []string{"vaultwarden", "backup", "proxy"} {
		if container := fixture.harness.composeServiceContainer(service, true); container != "" {
			fixture.t.Fatalf("accept decision unexpectedly started terminal service %s", service)
		}
	}

	resumed := runUpgradeResult(
		fixture.t,
		fixture.harness.upgradeCommand("upgrade-b", "upgrade", "accept"),
		productupgrade.PhaseAccepted,
		fixture.from,
		fixture.to,
	)
	if resumed.UpgradeID != accepted.UpgradeID {
		fixture.t.Fatal("replayed accept changed the immutable terminal decision")
	}
	fixture.harness.upgradeComposeCommand(
		true,
		"up",
		"--detach",
		"--wait",
		"--wait-timeout",
		"180",
		"vaultwarden",
		"backup",
		"proxy",
	)
	assertTerminalServices(fixture.t, fixture.harness, fixture.to, true)
	fixture.harness.resetUpgradeDrill()
	runUpgradeResult(
		fixture.t,
		fixture.harness.upgradeCommand("upgrade-b", "upgrade", "cleanup-drill"),
		productupgrade.PhaseAccepted,
		fixture.from,
		fixture.to,
	)
	runUpgradeResult(
		fixture.t,
		fixture.harness.upgradeCommand("upgrade-b", "upgrade", "finalize"),
		productupgrade.PhaseAccepted,
		fixture.from,
		fixture.to,
	)
	fixture.assertArchived(productupgrade.DecisionAccept)
	validateUpgradeReleaseImages(fixture.t, fixture.harness, &fixture.from)
	fixture.harness.setBackend(fixture.harness.mappedComposeServiceEndpoint("proxy", 8080))
	fixture.harness.waitHTTP(fixture.harness.publicHTTP+"/.vaultwarden-less/healthz", 204, time.Minute)
	client := newBitwardenCLI(
		fixture.t,
		filepath.Join(fixture.harness.localRoot, "bw-upgrade-accepted"),
		fixture.harness.publicTLS,
		fixture.harness.tlsCAFile,
		fixture.email,
	)
	client.login(fixture.ctx)
	if itemPassword(fixture.t, client.getItem(fixture.ctx, fixture.itemID)) != initialItemPassword {
		fixture.t.Fatal("accepted B could not read A data")
	}
	newItem := client.createItem(fixture.ctx, "Accepted B Writable Item", newItemPassword, "")
	if itemPassword(fixture.t, client.getItem(fixture.ctx, newItem)) != newItemPassword {
		fixture.t.Fatal("accepted B was not writable")
	}
}

func (fixture *upgradeFixture) rollbackAfterExchangeInterruption() {
	fixture.t.Helper()
	fixture.assertFrozenFailureState()
	rolledBack := runUpgradeResult(
		fixture.t,
		fixture.harness.upgradeCommand("rollback-a", "rollback", "apply"),
		productupgrade.PhaseRolledBack,
		fixture.from,
		fixture.to,
	)
	if len(rolledBack.Targets) != 0 {
		fixture.t.Fatalf("rollback unexpectedly returned targets: %#v", rolledBack.Targets)
	}
	fixture.assertTerminalDecision(productupgrade.DecisionRollback)
	activeMarker := filepath.Join(fixture.harness.localRoot, "data", fixture.markerName)
	if _, err := os.Lstat(activeMarker); !os.IsNotExist(err) {
		fixture.t.Fatalf("candidate-only marker remained active after exchange: %v", err)
	}
	rollbackMarker := filepath.Join(
		fixture.harness.localRoot,
		"state",
		"upgrade",
		"current",
		"rollback-data",
		fixture.markerName,
	)
	if value, err := os.ReadFile(rollbackMarker); err != nil || string(value) != "candidate-only\n" {
		fixture.t.Fatalf("exchange did not move B data to rollback position: %q/%v", value, err)
	}
	rsa, err := os.ReadFile(filepath.Join(fixture.harness.localRoot, "data", "rsa_key.pem"))
	if err != nil || sha256.Sum256(rsa) != fixture.originalRSA {
		fixture.t.Fatalf("exchange did not restore A RSA identity: %v", err)
	}
	if !contentIDPattern.MatchString(fixture.rollbackTreeID) {
		fixture.t.Fatal("frozen A rollback tree identity is invalid")
	}

	resumed := runUpgradeResult(
		fixture.t,
		fixture.harness.upgradeCommand("rollback-a", "rollback", "apply"),
		productupgrade.PhaseRolledBack,
		fixture.from,
		fixture.to,
	)
	if resumed.UpgradeID != rolledBack.UpgradeID {
		fixture.t.Fatal("replayed rollback changed the immutable terminal decision")
	}
	fixture.harness.selectProductionRelease(fixture.from)
	fixture.harness.compose(
		"up",
		"--detach",
		"--wait",
		"--wait-timeout",
		"180",
		"vaultwarden",
		"backup",
		"proxy",
	)
	assertTerminalServices(fixture.t, fixture.harness, fixture.from, false)
	fixture.harness.resetUpgradeDrill()
	runUpgradeResult(
		fixture.t,
		fixture.harness.upgradeCommand("rollback-a", "upgrade", "cleanup-drill"),
		productupgrade.PhaseRolledBack,
		fixture.from,
		fixture.to,
	)
	runUpgradeResult(
		fixture.t,
		fixture.harness.upgradeCommand("rollback-a", "upgrade", "finalize"),
		productupgrade.PhaseRolledBack,
		fixture.from,
		fixture.to,
	)
	archive := fixture.assertArchived(productupgrade.DecisionRollback)
	if value, err := os.ReadFile(filepath.Join(archive, "rollback-data", fixture.markerName)); err != nil || string(value) != "candidate-only\n" {
		fixture.t.Fatalf("B data was not quarantined after rollback: %q/%v", value, err)
	}
	if _, err := os.Stat(filepath.Join(archive, "candidate.json")); err != nil {
		fixture.t.Fatalf("verified B candidate evidence was not quarantined: %v", err)
	}
	var ticket productupgrade.RollbackTicket
	if err := decodeExactJSON(readPrivateText(fixture.t, filepath.Join(archive, "rollback-ticket.json")), &ticket); err != nil ||
		ticket.RollbackTreeID != fixture.rollbackTreeID {
		fixture.t.Fatalf("archived rollback tree identity changed: %#v/%v", ticket, err)
	}
	rsa, err = os.ReadFile(filepath.Join(fixture.harness.localRoot, "data", "rsa_key.pem"))
	if err != nil || sha256.Sum256(rsa) != fixture.originalRSA {
		fixture.t.Fatalf("terminal A RSA identity changed after rollback: %v", err)
	}

	fixture.harness.setBackend(fixture.harness.mappedComposeServiceEndpoint("proxy", 8080))
	fixture.harness.waitHTTP(fixture.harness.publicHTTP+"/.vaultwarden-less/healthz", 204, time.Minute)
	if itemPassword(fixture.t, fixture.client.getItem(fixture.ctx, fixture.itemID)) != initialItemPassword {
		fixture.t.Fatal("resumed A session could not read restored data")
	}
	restored := newBitwardenCLI(
		fixture.t,
		filepath.Join(fixture.harness.localRoot, "bw-upgrade-rolled-back"),
		fixture.harness.publicTLS,
		fixture.harness.tlsCAFile,
		fixture.email,
	)
	restored.login(fixture.ctx)
	if itemPassword(fixture.t, restored.getItem(fixture.ctx, fixture.itemID)) != initialItemPassword {
		fixture.t.Fatal("fresh client could not read A data after deepest rollback")
	}
}

func (h *e2eHarness) resetUpgradeDrill() {
	h.t.Helper()
	_, _ = h.tryUpgradeCompose(
		h.ctx, true, "--profile", "drill", "rm", "--force", "--stop", "drill-vaultwarden", "drill-copy",
	)
	volume := h.project + "_drill-data"
	if output, err := h.docker("volume", "rm", volume); err != nil &&
		!strings.Contains(strings.ToLower(output), "no such volume") {
		h.t.Fatalf("remove disposable upgrade drill volume: %v: %s", err, harnessDiagnostics(output))
	}
}

func (h *e2eHarness) composeServiceContainer(service string, runningOnly bool) string {
	h.t.Helper()
	arguments := []string{
		"ps", "--all", "--no-trunc", "--quiet",
		"--filter", "label=com.docker.compose.project=" + h.project,
		"--filter", "label=com.docker.compose.service=" + service,
	}
	if runningOnly {
		arguments = []string{
			"ps", "--no-trunc", "--quiet",
			"--filter", "label=com.docker.compose.project=" + h.project,
			"--filter", "label=com.docker.compose.service=" + service,
		}
	}
	output, err := h.docker(arguments...)
	if err != nil {
		h.t.Fatalf("locate Compose service %s: %v: %s", service, err, harnessDiagnostics(output))
	}
	ids := strings.Fields(output)
	if len(ids) > 1 || len(ids) == 1 && !lowerHex64Pattern.MatchString(ids[0]) {
		h.t.Fatalf("invalid Compose service identity for %s: %q", service, output)
	}
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func (h *e2eHarness) waitComposeServiceHealthy(service string, timeout time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		container := h.composeServiceContainer(service, true)
		if container != "" {
			output, err := h.docker("inspect", "--format", "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}", container)
			last = output
			if err == nil && strings.TrimSpace(output) == "running healthy" {
				return
			}
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(time.Second):
		}
	}
	h.t.Fatalf("Compose service %s did not become healthy: %s", service, harnessDiagnostics(last))
}

func (h *e2eHarness) mappedComposeServiceEndpoint(service string, port int) string {
	h.t.Helper()
	container := h.composeServiceContainer(service, true)
	if container == "" {
		h.t.Fatalf("Compose service %s is not running", service)
	}
	output, err := h.docker("inspect", "--format", "{{json .NetworkSettings.Ports}}", container)
	if err != nil {
		h.t.Fatalf("inspect Compose service port: %v: %s", err, harnessDiagnostics(output))
	}
	var ports map[string][]struct {
		HostIP   string `json:"HostIp"`
		HostPort string `json:"HostPort"`
	}
	if err := decodeExactJSON(output, &ports); err != nil {
		h.t.Fatalf("decode Compose service ports: %v", err)
	}
	bindings := ports[strconv.Itoa(port)+"/tcp"]
	if len(bindings) != 1 || bindings[0].HostIP != "127.0.0.1" {
		h.t.Fatalf("Compose service %s has an invalid loopback binding: %#v", service, bindings)
	}
	parsed, err := strconv.Atoi(bindings[0].HostPort)
	if err != nil || parsed < 1 || parsed > 65535 {
		h.t.Fatalf("Compose service %s has an invalid host port", service)
	}
	return "http://" + bindings[0].HostIP + ":" + bindings[0].HostPort
}

func (h *e2eHarness) assertProxyClosed() {
	h.t.Helper()
	output, err := h.docker(
		"ps", "--no-trunc", "--quiet", "--filter", "label=com.docker.compose.project="+h.project,
	)
	if err != nil {
		h.t.Fatalf("list running project containers: %v: %s", err, harnessDiagnostics(output))
	}
	for _, container := range strings.Fields(output) {
		inspect, err := h.docker("inspect", container)
		if err != nil {
			h.t.Fatalf("inspect running project container: %v: %s", err, harnessDiagnostics(inspect))
		}
		var documents []struct {
			Config struct {
				Labels map[string]string `json:"Labels"`
			} `json:"Config"`
			NetworkSettings struct {
				Ports map[string][]json.RawMessage `json:"Ports"`
			} `json:"NetworkSettings"`
		}
		if err := json.Unmarshal([]byte(inspect), &documents); err != nil || len(documents) != 1 {
			h.t.Fatalf("decode running project container: %v", err)
		}
		service := documents[0].Config.Labels["com.docker.compose.service"]
		if strings.Contains(service, "proxy") || len(documents[0].NetworkSettings.Ports["8080/tcp"]) != 0 {
			h.t.Fatalf("proxy exposure remained open through upgrade failure: %s", service)
		}
	}
}
