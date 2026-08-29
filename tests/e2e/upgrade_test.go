package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	productcli "github.com/hellodword/vaultwarden-less/internal/cli"
	productupgrade "github.com/hellodword/vaultwarden-less/internal/upgrade"
)

type upgradeRelease struct {
	commit             string
	release            string
	vaultwardenVersion string
}

type upgradeFixture struct {
	t               *testing.T
	ctx             context.Context
	harness         *e2eHarness
	from            upgradeRelease
	to              upgradeRelease
	upgradeID       string
	email           string
	itemID          string
	client          *bitwardenCLI
	originalRSA     [sha256.Size]byte
	ticket          []byte
	markerName      string
	backupConfig    string
	expectCandidate bool
}

func TestUpgradeRollbackLifecycle(t *testing.T) {
	if !*runUpgradeE2E {
		t.Skip("pass -run-upgrade-e2e with two full clean commits to run the real upgrade lifecycle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 105*time.Minute)
	defer cancel()
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

	t.Run("migration-permission-failure-explicit-rollback", func(t *testing.T) {
		fixture := newUpgradeFixture(t, ctx, from, to, false)
		fixture.forceMigrationPermissionFailure()
		fixture.verifyFailureAndRollback(false)
	})
	t.Run("invalid-target-verification-failure-explicit-rollback", func(t *testing.T) {
		fixture := newUpgradeFixture(t, ctx, from, to, false)
		fixture.startCandidateAndRecordMigration()
		fixture.forceInvalidTargetVerificationFailure()
		fixture.verifyFailureAndRollback(true)
	})
	t.Run("corrupt-drill-failure-explicit-rollback", func(t *testing.T) {
		fixture := newUpgradeFixture(t, ctx, from, to, false)
		fixture.startCandidateAndRecordMigration()
		fixture.verifyCandidate()
		fixture.forceCorruptDrillFailure()
		fixture.verifyFailureAndRollback(true)
	})
	t.Run("isolated-evidence-and-accept-boundary", func(t *testing.T) {
		fixture := newUpgradeFixture(t, ctx, from, to, true)
		fixture.startCandidateAndRecordMigration()
		fixture.verifyCandidate()
		fixture.acceptAndVerifyBoundary()
	})
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
	preflight bool,
) *upgradeFixture {
	t.Helper()
	harness := newE2EHarnessWithImages(t, ctx, false)
	harness.setDockerEnvironment("A_RELEASE", from.release)
	harness.setDockerEnvironment("B_RELEASE", to.release)
	harness.validateUpgradeRollbackComposeModel()
	harness.selectProductionRelease(from)
	harness.startMotoAndRepository()
	harness.startVaultwarden()

	unique := time.Now().UnixNano()
	email := fmt.Sprintf("vaultwarden-less-e2e-upgrade-%d@example.com", unique)
	_ = registerAccount(ctx, t, harness.publicHTTP, email, testPassword)
	client := newBitwardenCLI(t, filepath.Join(harness.localRoot, "bw-upgrade-a"), harness.publicTLS, harness.tlsCAFile, email)
	client.login(ctx)
	itemID := client.createItem(ctx, "Upgrade Recovery Item", initialItemPassword, "")
	harness.startBackup()
	snapshots := harness.waitSnapshotCount(1, 5*time.Minute)
	harness.startProxy()

	if preflight {
		restored := harness.restore(snapshots.Snapshots[0].ID, "upgrade-preflight")
		assertRestoreMatchesSnapshot(t, restored, snapshots.Snapshots[0])
		harness.selectProductionRelease(to)
		harness.startDrill("upgrade-preflight")
		preflightClient := newBitwardenCLI(t, filepath.Join(harness.localRoot, "bw-upgrade-preflight"), harness.publicTLS, harness.tlsCAFile, email)
		preflightClient.login(ctx)
		if itemPassword(t, preflightClient.getItem(ctx, itemID)) != initialItemPassword {
			t.Fatal("isolated B migration did not preserve A data")
		}
		_ = preflightClient.createItem(ctx, "Disposable B Preflight Write", secondPassword, "")
		harness.stopDrill()
		if output, err := harness.docker("volume", "rm", harness.project+"_drill-data"); err != nil {
			t.Fatalf("remove disposable preflight volume: %v: %s", err, harnessDiagnostics(output))
		}
		harness.selectProductionRelease(from)
		harness.setBackend(harness.mappedEndpoint("proxy", 8080))
		harness.waitHTTP(harness.publicHTTP+"/.vaultwarden-less/healthz", 204, time.Minute)
	}

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
		originalRSA: sha256.Sum256(rsaBytes), ticket: ticket, markerName: markerName,
		backupConfig: readPrivateText(t, filepath.Join(harness.localRoot, "config", "backup.yaml")),
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
		"--file", h.testCompose,
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
	if err := decodeExactJSON(output, &result); err != nil || result.SchemaVersion != productupgrade.SchemaVersion ||
		result.Phase != phase || result.FromCommit != from.commit || result.ToCommit != to.commit ||
		result.UpgradeID != from.commit+".."+to.commit || result.Targets == nil {
		t.Fatalf("invalid %s upgrade result: %v: %s", phase, err, harnessDiagnostics(output))
	}
	return result
}

func (fixture *upgradeFixture) startCandidateAndRecordMigration() {
	fixture.t.Helper()
	fixture.harness.upgradeComposeCommand(true, "up", "--detach", "vaultwarden")
	fixture.harness.waitComposeServiceHealthy("vaultwarden", 3*time.Minute)
	fixture.harness.setBackend(fixture.harness.mappedComposeServiceEndpoint("vaultwarden", 80))
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
	fixture.expectCandidate = true
	fixture.harness.upgradeComposeCommand(true, "stop", "vaultwarden")
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

func (fixture *upgradeFixture) forceMigrationPermissionFailure() {
	fixture.t.Helper()
	database := filepath.Join(fixture.harness.localRoot, "data", "db.sqlite3")
	if err := os.Chmod(database, 0o400); err != nil {
		fixture.t.Fatal(err)
	}
	info, err := os.Stat(database)
	if err != nil || info.Mode().Perm() != 0o400 {
		fixture.t.Fatalf("database did not become read-only: %v", err)
	}
	fixture.harness.upgradeComposeCommand(true, "up", "--detach", "vaultwarden")
	logs := fixture.harness.waitComposeServiceFailure("vaultwarden", 90*time.Second)
	lower := strings.ToLower(logs)
	if !strings.Contains(lower, "permission") && !strings.Contains(lower, "read-only") &&
		!strings.Contains(lower, "readonly") && !strings.Contains(lower, "unable to open database") {
		fixture.t.Fatalf("candidate migration failed without a permission diagnostic: %s", harnessDiagnostics(logs))
	}
	fixture.harness.upgradeComposeCommand(true, "stop", "vaultwarden")
	if err := os.Chmod(database, 0o600); err != nil {
		fixture.t.Fatal(err)
	}
}

func (fixture *upgradeFixture) forceInvalidTargetVerificationFailure() {
	fixture.t.Helper()
	path := filepath.Join(fixture.harness.localRoot, "config", "backup.yaml")
	invalid := strings.Replace(
		fixture.backupConfig,
		"repository: s3:http://moto-primary:5000/vaultwarden-less-e2e-primary/repository",
		"repository: unsupported://upgrade-e2e",
		1,
	)
	if invalid == fixture.backupConfig {
		fixture.t.Fatal("could not replace the verification target with an invalid backend")
	}
	writePrivateFile(fixture.t, path, invalid)
	fixture.expectUpgradeFailure("repository_failure", "upgrade-b", "upgrade", "verify")
	writePrivateFile(fixture.t, path, fixture.backupConfig)
}

func (fixture *upgradeFixture) forceCorruptDrillFailure() {
	fixture.t.Helper()
	manifest := filepath.Join(fixture.harness.localRoot, "restore", "drill", "manifest.json")
	writePrivateFile(fixture.t, manifest, "{\"schema_version\":1,\"corrupt\":true}\n")
	fixture.harness.resetUpgradeDrill()
	_, _ = fixture.harness.tryUpgradeCompose(
		fixture.ctx, true, "--profile", "drill", "up", "--detach", "drill-vaultwarden",
	)
	_ = fixture.harness.waitComposeServiceFailure("drill-copy", 90*time.Second)
	fixture.expectUpgradeFailure("verification_failure", "upgrade-b", "upgrade", "accept")
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
		SchemaVersion int `json:"schema_version"`
		Error         struct {
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
		if decodeExactJSON(line, &document) == nil && document.SchemaVersion == productcli.ResultSchemaVersion &&
			document.Error.Code != "" && document.Error.Message != "" {
			codes = append(codes, document.Error.Code)
		}
	}
	if len(codes) != 1 {
		t.Fatalf("failure did not contain exactly one strict command error: %s", harnessDiagnostics(output))
	}
	return codes[0]
}

func (fixture *upgradeFixture) verifyFailureAndRollback(expectCandidate bool) {
	fixture.t.Helper()
	if fixture.expectCandidate != expectCandidate {
		fixture.t.Fatalf("candidate expectation = %v, want %v", fixture.expectCandidate, expectCandidate)
	}
	fixture.assertFrozenFailureState()
	result := runUpgradeResult(
		fixture.t,
		fixture.harness.upgradeCommand("rollback-a", "rollback", "apply"),
		productupgrade.PhaseRolledBack,
		fixture.from,
		fixture.to,
	)
	if len(result.Targets) != 0 {
		fixture.t.Fatalf("rollback unexpectedly returned targets: %#v", result.Targets)
	}
	fixture.assertRollbackFiles(expectCandidate)
	fixture.startRollbackServicesAndReadData()
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

func (fixture *upgradeFixture) assertRollbackFiles(expectCandidate bool) {
	fixture.t.Helper()
	current := filepath.Join(fixture.harness.localRoot, "state", "upgrade", "current")
	if _, err := os.Lstat(current); !os.IsNotExist(err) {
		fixture.t.Fatalf("current upgrade state remained after rollback: %v", err)
	}
	history := filepath.Join(fixture.harness.localRoot, "state", "upgrade", "history", fixture.upgradeID)
	if info, err := os.Stat(history); err != nil || !info.IsDir() {
		fixture.t.Fatalf("rollback history is unavailable: %v", err)
	}
	activeMarker := filepath.Join(fixture.harness.localRoot, "data", fixture.markerName)
	if _, err := os.Lstat(activeMarker); !os.IsNotExist(err) {
		fixture.t.Fatalf("candidate-only marker remained active after rollback: %v", err)
	}
	quarantine := filepath.Join(
		fixture.harness.localRoot, "state", "upgrade", "quarantine", fixture.upgradeID,
	)
	if value, err := os.ReadFile(filepath.Join(quarantine, "data", fixture.markerName)); err != nil || string(value) != "candidate-only\n" {
		fixture.t.Fatalf("candidate data was not quarantined: %q/%v", value, err)
	}
	candidate := filepath.Join(quarantine, "candidate.json")
	if _, err := os.Lstat(candidate); expectCandidate && err != nil || !expectCandidate && !os.IsNotExist(err) {
		fixture.t.Fatalf("quarantined candidate state mismatch: %v", err)
	}
}

func (fixture *upgradeFixture) startRollbackServicesAndReadData() {
	fixture.t.Helper()
	fixture.harness.upgradeComposeCommand(
		false, "--profile", "rollback", "up", "--detach",
		"rollback-vaultwarden-a", "rollback-backup-a", "rollback-proxy-a",
	)
	fixture.harness.waitComposeServiceHealthy("rollback-proxy-a", 4*time.Minute)
	fixture.harness.setBackend(fixture.harness.mappedComposeServiceEndpoint("rollback-proxy-a", 8080))
	fixture.harness.waitHTTP(fixture.harness.publicHTTP+"/.vaultwarden-less/healthz", 204, time.Minute)
	fixture.client.expectSyncFailure(fixture.ctx)
	restored := newBitwardenCLI(
		fixture.t,
		filepath.Join(fixture.harness.localRoot, "bw-upgrade-rolled-back"),
		fixture.harness.publicTLS,
		fixture.harness.tlsCAFile,
		fixture.email,
	)
	restored.login(fixture.ctx)
	if itemPassword(fixture.t, restored.getItem(fixture.ctx, fixture.itemID)) != initialItemPassword {
		fixture.t.Fatal("A data was not readable after explicit rollback")
	}
	rsaPath := filepath.Join(fixture.harness.localRoot, "data", "rsa_key.pem")
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(rsaPath)
		if err == nil && len(data) != 0 && sha256.Sum256(data) != fixture.originalRSA {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	fixture.t.Fatal("A rollback did not regenerate the RSA identity")
}

func (fixture *upgradeFixture) acceptAndVerifyBoundary() {
	fixture.t.Helper()
	fixture.harness.resetUpgradeDrill()
	fixture.harness.upgradeComposeCommand(true, "--profile", "drill", "up", "--detach", "drill-vaultwarden")
	fixture.harness.waitComposeServiceHealthy("drill-vaultwarden", 4*time.Minute)
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
	_, _ = fixture.harness.tryUpgradeCompose(
		fixture.ctx, false, "--profile", "drill", "rm", "--force", "--stop", "drill-vaultwarden", "drill-copy",
	)
	fixture.harness.upgradeComposeCommand(false, "up", "--detach", "vaultwarden", "backup", "proxy")
	fixture.harness.waitComposeServiceHealthy("proxy", 4*time.Minute)
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

	base := []string{"--profile", "upgrade", "run", "--rm", "--no-deps", "rollback-a", "rollback", "apply"}
	output, err := fixture.harness.tryUpgradeCompose(fixture.ctx, false, base...)
	if err == nil || exactCommandErrorCode(fixture.t, output) != "conflict" {
		fixture.t.Fatalf("ordinary rollback was not rejected after accept: %v: %s", err, harnessDiagnostics(output))
	}
	fixture.harness.waitComposeServiceHealthy("proxy", time.Minute)
	if itemPassword(fixture.t, client.getItem(fixture.ctx, newItem)) != newItemPassword {
		fixture.t.Fatal("rejected rollback changed accepted B data")
	}
	if _, err := os.Stat(filepath.Join(fixture.harness.localRoot, "state", "upgrade", "history", fixture.upgradeID)); err != nil {
		fixture.t.Fatalf("accepted history is unavailable: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.harness.localRoot, "state", "upgrade", "quarantine", fixture.upgradeID)); !os.IsNotExist(err) {
		fixture.t.Fatalf("successful accept created rollback quarantine: %v", err)
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

func (h *e2eHarness) waitComposeServiceFailure(service string, timeout time.Duration) string {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for time.Now().Before(deadline) {
		container := h.composeServiceContainer(service, false)
		if container != "" {
			output, _ := h.docker("inspect", "--format", "{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}", container)
			last = output
			if strings.TrimSpace(output) == "running healthy" {
				h.t.Fatalf("Compose service %s unexpectedly became healthy", service)
			}
			fields := strings.Fields(output)
			if len(fields) > 0 && (fields[0] == "exited" || fields[0] == "restarting" || strings.Contains(output, "unhealthy")) {
				logs, _ := h.docker("logs", "--tail", "100", container)
				return logs
			}
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatal(h.ctx.Err())
		case <-time.After(time.Second):
		}
	}
	h.t.Fatalf("Compose service %s did not reach a failed state: %s", service, harnessDiagnostics(last))
	return ""
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
