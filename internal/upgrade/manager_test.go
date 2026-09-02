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
	"github.com/hellodword/vaultwarden-less/internal/deployment"
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
	restore    string
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
		repeated.UpgradeID != prepared.UpgradeID ||
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
	if err := fixture.serving(testACommit, testARelease); !IsKind(err, ErrorConflict) {
		t.Fatalf("frozen A serve = %v", err)
	}
	if err := fixture.serving(testBCommit, testBRelease); !IsKind(err, ErrorConflict) {
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
	if _, err := os.Lstat(state.current); err != nil {
		t.Fatalf("current was archived before services and cleanup: %v", err)
	}
	if err := fixture.serving(testBCommit, testBRelease); err != nil {
		t.Fatalf("accepted B serve: %v", err)
	}
	if _, err := fixture.manager(testACommit, testARelease).Rollback(context.Background()); !IsKind(err, ErrorConflict) {
		t.Fatalf("post-accept rollback error = %v", err)
	}
	finalized, err := fixture.manager(testBCommit, testBRelease).Finalize(context.Background())
	if err != nil || finalized.Phase != PhaseAccepted {
		t.Fatalf("accept finalize = %#v/%v", finalized, err)
	}
	if _, err := os.Lstat(state.current); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("current remains after finalize: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(state.history, prepared.UpgradeID, decisionName)); err != nil {
		t.Fatalf("accepted decision was not archived: %v", err)
	}
}

func TestTerminalDecisionFaultMatrixIsResumableAndConflictingDecisionIsRejected(t *testing.T) {
	t.Run("accept decision survives activation failure", func(t *testing.T) {
		fixture := newManagerFixture(t)
		prepareAndMigrate(t, fixture)
		b := fixture.manager(testBCommit, testBRelease)
		if _, err := b.Verify(context.Background(), []string{"primary"}, fixedCollector(evidence([]string{"primary"}, "a"))); err != nil {
			t.Fatal(err)
		}
		drillCalls := 0
		b.Activate = func(expected, target deployment.Identity, notBefore time.Time) error {
			drillCalls++
			if expected.ProjectCommit != testACommit || target.ProjectCommit != testBCommit {
				t.Fatalf("activation identities = %#v/%#v", expected, target)
			}
			if notBefore.IsZero() {
				t.Fatal("accept activation did not carry the decision instant")
			}
			return errors.New("synthetic activation interruption")
		}
		if _, err := b.Accept(context.Background(), func(context.Context) error { return nil }); !IsKind(err, ErrorInvalidState) {
			t.Fatalf("interrupted accept = %v", err)
		}
		state := paths(fixture.state)
		decisionBefore, err := os.ReadFile(state.decision)
		if err != nil {
			t.Fatal(err)
		}
		prepare, err := readPrepare(state, fixture.deployment)
		if err != nil {
			t.Fatal(err)
		}
		decision, err := readDecision(state, prepare)
		if err != nil || decision.Decision != DecisionAccept {
			t.Fatalf("accept decision = %#v/%v", decision, err)
		}
		active, err := deployment.ReadActive(fixture.state)
		if err != nil || active.ProjectCommit != testACommit {
			t.Fatalf("active before resumed accept = %#v/%v", active, err)
		}
		if decision.DecidedAt <= active.ActivatedAt {
			t.Fatalf("decision instant %q did not follow prior activation %q", decision.DecidedAt, active.ActivatedAt)
		}
		if _, err := fixture.manager(testACommit, testARelease).Rollback(context.Background()); !IsKind(err, ErrorConflict) {
			t.Fatalf("rollback after accept decision = %v", err)
		}

		resumed := fixture.manager(testBCommit, testBRelease)
		result, err := resumed.Accept(context.Background(), func(context.Context) error {
			t.Fatal("resumed accept repeated the pre-decision drill")
			return nil
		})
		if err != nil || result.Phase != PhaseAccepted || drillCalls != 1 {
			t.Fatalf("resumed accept = %#v/%v activation calls=%d", result, err, drillCalls)
		}
		active, err = deployment.ReadActive(fixture.state)
		if err != nil || active.ProjectCommit != testBCommit || active.Release != testBRelease {
			t.Fatalf("active after resumed accept = %#v/%v", active, err)
		}
		if active.ActivatedAt < decision.DecidedAt {
			t.Fatalf("terminal activation %q predates decision %q", active.ActivatedAt, decision.DecidedAt)
		}
		decisionAfter, err := os.ReadFile(state.decision)
		if err != nil || !bytes.Equal(decisionBefore, decisionAfter) {
			t.Fatalf("accept decision was rewritten = %q/%v", decisionAfter, err)
		}
	})

	t.Run("rollback exchange survives activation failure", func(t *testing.T) {
		fixture := newManagerFixture(t)
		a := fixture.manager(testACommit, testARelease)
		if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Freeze(context.Background(), []string{"primary"}, fixedCollector(evidence([]string{"primary"}, "b"))); err != nil {
			t.Fatal(err)
		}
		writePrivateFile(t, filepath.Join(fixture.data, "b-only"), []byte("candidate B"))
		rollback := fixture.manager(testACommit, testARelease)
		activationCalls := 0
		rollback.Activate = func(expected, target deployment.Identity, notBefore time.Time) error {
			activationCalls++
			if expected != target || target.ProjectCommit != testACommit {
				t.Fatalf("rollback activation identities = %#v/%#v", expected, target)
			}
			if notBefore.IsZero() {
				t.Fatal("rollback activation did not carry the decision instant")
			}
			return errors.New("synthetic post-exchange interruption")
		}
		if _, err := rollback.Rollback(context.Background()); !IsKind(err, ErrorInvalidState) {
			t.Fatalf("interrupted rollback = %v", err)
		}
		state := paths(fixture.state)
		decisionBefore, err := os.ReadFile(state.decision)
		if err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(filepath.Join(state.rollback, "b-only")); err != nil || string(data) != "candidate B" {
			t.Fatalf("post-exchange B tree = %q/%v", data, err)
		}
		if err := fixture.serving(testACommit, testARelease); !IsKind(err, ErrorConflict) {
			t.Fatalf("A served before rollback activation was reaffirmed: %v", err)
		}
		resumed := fixture.manager(testACommit, testARelease)
		resumed.Exchange = func(string, string) error {
			t.Fatal("resumed rollback repeated the exchange")
			return nil
		}
		result, err := resumed.Rollback(context.Background())
		if err != nil || result.Phase != PhaseRolledBack || activationCalls != 1 {
			t.Fatalf("resumed rollback = %#v/%v activation calls=%d", result, err, activationCalls)
		}
		decisionAfter, err := os.ReadFile(state.decision)
		if err != nil || !bytes.Equal(decisionBefore, decisionAfter) {
			t.Fatalf("rollback decision was rewritten = %q/%v", decisionAfter, err)
		}
		if _, err := fixture.manager(testBCommit, testBRelease).Accept(context.Background(), func(context.Context) error {
			t.Fatal("conflicting accept performed a drill check")
			return nil
		}); !IsKind(err, ErrorConflict) {
			t.Fatalf("accept after rollback decision = %v", err)
		}
	})
}

func TestTerminalStateWriteTransientsAreValidatedAndRecovered(t *testing.T) {
	t.Run("accept", func(t *testing.T) {
		fixture := newManagerFixture(t)
		prepareAndMigrate(t, fixture)
		b := fixture.manager(testBCommit, testBRelease)
		if _, err := b.Verify(context.Background(), []string{"primary"}, fixedCollector(evidence([]string{"primary"}, "a"))); err != nil {
			t.Fatal(err)
		}
		state := paths(fixture.state)
		operatorEntry := filepath.Join(state.current, ".vaultwarden-less-state-"+decisionName+"-operator")
		writePrivateFile(t, operatorEntry, []byte("not upgrade-owned"))
		if _, err := b.Accept(context.Background(), func(context.Context) error { return nil }); !IsKind(err, ErrorInvalidState) {
			t.Fatalf("similarly named unexpected entry = %v", err)
		}
		if data, err := os.ReadFile(operatorEntry); err != nil || string(data) != "not upgrade-owned" {
			t.Fatalf("unexpected entry was removed or changed: %q/%v", data, err)
		}
		if err := os.Remove(operatorEntry); err != nil {
			t.Fatal(err)
		}

		transient := filepath.Join(state.current, ".vaultwarden-less-state-"+decisionName+"-interrupted.tmp")
		writePrivateFile(t, transient, []byte("partial decision"))
		if err := CandidateServingAllowed(
			fixture.state, testBCommit, testBRelease, testACommit, testARelease, fixture.deployment,
		); err != nil {
			t.Fatalf("safe interrupted write blocked candidate gate: %v", err)
		}
		if err := os.Chmod(transient, 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Accept(context.Background(), func(context.Context) error { return nil }); !IsKind(err, ErrorInvalidState) {
			t.Fatalf("unsafe reserved transient = %v", err)
		}
		if _, err := os.Lstat(transient); err != nil {
			t.Fatalf("unsafe reserved transient was removed: %v", err)
		}
		if err := os.Chmod(transient, 0o600); err != nil {
			t.Fatal(err)
		}
		accepted, err := b.Accept(context.Background(), func(context.Context) error { return nil })
		if err != nil || accepted.Phase != PhaseAccepted {
			t.Fatalf("accept after interrupted state write = %#v/%v", accepted, err)
		}
		if _, err := os.Lstat(transient); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("decision transient survived accept retry: %v", err)
		}
	})

	t.Run("rollback", func(t *testing.T) {
		fixture := newManagerFixture(t)
		a := fixture.manager(testACommit, testARelease)
		if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Freeze(context.Background(), []string{"primary"}, fixedCollector(evidence([]string{"primary"}, "b"))); err != nil {
			t.Fatal(err)
		}
		writePrivateFile(t, filepath.Join(fixture.data, "b-only"), []byte("candidate B"))
		state := paths(fixture.state)
		transient := filepath.Join(state.current, ".vaultwarden-less-state-"+decisionName+"-interrupted.tmp")
		writePrivateFile(t, transient, []byte("partial decision"))
		rolledBack, err := a.Rollback(context.Background())
		if err != nil || rolledBack.Phase != PhaseRolledBack {
			t.Fatalf("rollback after interrupted state write = %#v/%v", rolledBack, err)
		}
		if _, err := os.Lstat(transient); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("decision transient survived rollback retry: %v", err)
		}
	})
}

func TestFinalizeRequiresExactDrillCleanup(t *testing.T) {
	fixture := newManagerFixture(t)
	prepareAndMigrate(t, fixture)
	b := fixture.manager(testBCommit, testBRelease)
	if _, err := b.Verify(context.Background(), []string{"primary"}, fixedCollector(evidence([]string{"primary"}, "c"))); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Accept(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	drill := filepath.Join(fixture.restore, "drill")
	if err := os.Mkdir(drill, 0o700); err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, filepath.Join(drill, "manifest.json"), []byte("synthetic verified drill"))
	if _, err := b.Finalize(context.Background()); !IsKind(err, ErrorConflict) {
		t.Fatalf("finalize before cleanup = %v", err)
	}
	cleaned := false
	b.RemoveDrill = func(_ context.Context, path, projectCommit string) error {
		if path != drill || projectCommit != testBCommit {
			t.Fatalf("cleanup boundary = %q/%q", path, projectCommit)
		}
		cleaned = true
		return os.RemoveAll(path)
	}
	if result, err := b.CleanupDrill(context.Background()); err != nil || result.Phase != PhaseAccepted || !cleaned {
		t.Fatalf("cleanup = %#v/%v cleaned=%t", result, err, cleaned)
	}
	if _, err := b.CleanupDrill(context.Background()); err != nil {
		t.Fatalf("idempotent cleanup = %v", err)
	}
	if _, err := b.Finalize(context.Background()); err != nil {
		t.Fatalf("finalize after cleanup = %v", err)
	}
}

func TestPrepareWritesExactComposeEnvironment(t *testing.T) {
	fixture := newManagerFixture(t)
	if _, err := fixture.manager(testACommit, testARelease).Prepare(testBCommit, testBRelease); err != nil {
		t.Fatal(err)
	}
	want := "DEPLOYMENT_ROOT=/srv/vaultwarden-less\n" +
		"RUNTIME_UID=" + strconv.Itoa(os.Geteuid()) + "\n" +
		"RUNTIME_GID=" + strconv.Itoa(os.Getegid()) + "\n" +
		"LISTEN_PORT=8080\n" +
		"DRILL_PORT=8081\n" +
		"VWLESS_IMAGE_TAG=" + testBRelease + "\n" +
		"A_RELEASE=" + testARelease + "\n" +
		"B_RELEASE=" + testBRelease + "\n"
	got, err := os.ReadFile(paths(fixture.state).compose)
	if err != nil || string(got) != want {
		t.Fatalf("compose.env = %q/%v, want %q", got, err, want)
	}
}

func TestCandidateAndUpgradeDrillServingGates(t *testing.T) {
	fixture := newManagerFixture(t)
	a := fixture.manager(testACommit, testARelease)
	if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
		t.Fatal(err)
	}
	if err := CandidateServingAllowed(
		fixture.state, testBCommit, testBRelease, testACommit, testARelease, fixture.deployment,
	); !IsKind(err, ErrorInvalidState) {
		t.Fatalf("unfrozen candidate gate = %v", err)
	}
	if _, err := a.Freeze(context.Background(), []string{"primary"}, fixedCollector(evidence([]string{"primary"}, "1"))); err != nil {
		t.Fatal(err)
	}
	if err := CandidateServingAllowed(
		fixture.state, testBCommit, testBRelease, testACommit, testARelease, fixture.deployment,
	); err != nil {
		t.Fatalf("frozen candidate gate = %v", err)
	}
	for _, identity := range []struct {
		candidateCommit  string
		candidateRelease string
		activeCommit     string
		activeRelease    string
	}{
		{testACommit, testARelease, testACommit, testARelease},
		{testBCommit, testBRelease, testBCommit, testBRelease},
		{testBCommit, "RELEASE.2026-08-27T13-00-00Z-gcccccccccccc", testACommit, testARelease},
	} {
		if err := CandidateServingAllowed(
			fixture.state, identity.candidateCommit, identity.candidateRelease,
			identity.activeCommit, identity.activeRelease, fixture.deployment,
		); !IsKind(err, ErrorConflict) && !IsKind(err, ErrorInvalidState) {
			t.Fatalf("mismatched candidate gate = %v", err)
		}
	}

	b := fixture.manager(testBCommit, testBRelease)
	if _, err := b.MigrateCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	verifiedEvidence := evidence([]string{"primary"}, "2")
	if _, err := b.Verify(context.Background(), []string{"primary"}, fixedCollector(verifiedEvidence)); err != nil {
		t.Fatal(err)
	}
	if err := DrillServingAllowed(
		fixture.state, testBCommit, testBRelease,
		verifiedEvidence[0].ContentID, verifiedEvidence[0].ArchiveID, fixture.deployment,
	); err != nil {
		t.Fatalf("verified upgrade drill gate = %v", err)
	}
	if err := DrillServingAllowed(
		fixture.state, testBCommit, testBRelease,
		"sha256:"+strings.Repeat("f", 64), verifiedEvidence[0].ArchiveID, fixture.deployment,
	); !IsKind(err, ErrorVerification) {
		t.Fatalf("mismatched upgrade drill gate = %v", err)
	}
	if err := DrillServingAllowed(
		fixture.state, testACommit, testARelease,
		verifiedEvidence[0].ContentID, verifiedEvidence[0].ArchiveID, fixture.deployment,
	); !IsKind(err, ErrorConflict) {
		t.Fatalf("A upgrade drill gate = %v", err)
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
		if _, err := os.Lstat(filepath.Join(fixture.data, rsa)); err != nil {
			t.Fatalf("restored A RSA state is missing at %s: %v", rsa, err)
		}
	}
	state := paths(fixture.state)
	quarantine := filepath.Join(state.quarantine, upgradeID(testACommit, testBCommit))
	if data, err := os.ReadFile(filepath.Join(state.rollback, "b-only")); err != nil || string(data) != "failed B data" {
		t.Fatalf("exchanged B data = %q/%v", data, err)
	}
	for _, current := range []string{state.current, state.candidate, state.decision} {
		if _, err := os.Lstat(current); err != nil {
			t.Fatalf("terminal current state was removed before finalize at %s: %v", current, err)
		}
	}
	if err := fixture.serving(testACommit, testARelease); err != nil {
		t.Fatalf("rolled-back A serve: %v", err)
	}
	if err := RollbackServingAllowed(
		context.Background(), fixture.state, fixture.data,
		deployment.Identity{ProjectCommit: testACommit, Release: testARelease}, fixture.deployment,
	); err != nil {
		t.Fatalf("rollback A gate: %v", err)
	}
	unmounted := fixture.data + "-unmounted-after-rollback"
	if err := os.Rename(fixture.data, unmounted); err != nil {
		t.Fatal(err)
	}
	if err := StateOnlyServingAllowed(
		context.Background(), fixture.state,
		deployment.Identity{ProjectCommit: testACommit, Release: testARelease}, fixture.deployment,
	); err != nil {
		t.Fatalf("rollback state-only gate required data: %v", err)
	}
	if err := os.Rename(unmounted, fixture.data); err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, filepath.Join(fixture.data, "post-start"), []byte("ordinary A runtime mutation"))
	if err := fixture.serving(testACommit, testARelease); err != nil {
		t.Fatalf("restarted A service after runtime mutation: %v", err)
	}
	finalized, err := fixture.manager(testACommit, testARelease).Finalize(context.Background())
	if err != nil || finalized.Phase != PhaseRolledBack {
		t.Fatalf("rollback finalize = %#v/%v", finalized, err)
	}
	if data, err := os.ReadFile(filepath.Join(quarantine, rollbackDataName, "b-only")); err != nil || string(data) != "failed B data" {
		t.Fatalf("quarantined B data = %q/%v", data, err)
	}
	for _, archived := range []string{candidateName, decisionName, ticketName} {
		if _, err := os.Lstat(filepath.Join(quarantine, archived)); err != nil {
			t.Fatalf("rollback archive missing %s: %v", archived, err)
		}
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
	if _, err := fixture.manager(testACommit, testARelease).Finalize(context.Background()); err != nil {
		t.Fatal(err)
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
	if _, err := fixture.manager(testACommit, testARelease).Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	quarantined := filepath.Join(paths(fixture.state).quarantine, upgradeID(testACommit, testBCommit), rollbackDataName, "future-b-link")
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
		writePrivateFile(t, state.prepare, []byte(`{"upgrade_id":"duplicate","upgrade_id":"duplicate"}`+"\n"))
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
			wantError: ErrorConflict,
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
			if err := fixture.serving(testACommit, testARelease); !IsKind(err, ErrorConflict) && !IsKind(err, ErrorInvalidState) && !IsKind(err, ErrorVerification) {
				t.Fatalf("serving gate after failed rollback = %v", err)
			}
		})
	}
}

func TestServingGateAllowsAOnlyBeforeFreezeAndFailsClosed(t *testing.T) {
	fixture := newManagerFixture(t)
	if err := fixture.serving(testACommit, testARelease); err != nil {
		t.Fatalf("fresh serve = %v", err)
	}
	a := fixture.manager(testACommit, testARelease)
	if _, err := a.Prepare(testBCommit, testBRelease); err != nil {
		t.Fatal(err)
	}
	if err := fixture.serving(testACommit, testARelease); err != nil {
		t.Fatalf("prepared A serve = %v", err)
	}
	if err := fixture.serving(testBCommit, testBRelease); !IsKind(err, ErrorConflict) {
		t.Fatalf("prepared B serve = %v", err)
	}
	state := paths(fixture.state)
	compose, err := os.ReadFile(state.compose)
	if err != nil {
		t.Fatal(err)
	}
	writePrivateFile(t, state.compose, bytes.Replace(compose, []byte("LISTEN_PORT=8080"), []byte("LISTEN_PORT=8082"), 1))
	if err := fixture.serving(testACommit, testARelease); !IsKind(err, ErrorInvalidState) {
		t.Fatalf("tampered compose serve = %v", err)
	}
	writePrivateFile(t, state.compose, compose)
	writePrivateFile(t, state.ticket, []byte(`{"unexpected":true}`+"\n"))
	if err := fixture.serving(testACommit, testARelease); !IsKind(err, ErrorInvalidState) {
		t.Fatalf("corrupt frozen serve = %v", err)
	}
}

func TestStateOnlyServingGateKeepsDataUnmountedAndFailClosed(t *testing.T) {
	fixture := newManagerFixture(t)
	prepareAndMigrate(t, fixture)
	b := fixture.manager(testBCommit, testBRelease)
	if _, err := b.Verify(
		context.Background(), []string{"primary"}, fixedCollector(evidence([]string{"primary"}, "9")),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Accept(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	identity := deployment.Identity{ProjectCommit: testBCommit, Release: testBRelease}
	if err := StateOnlyServingAllowed(context.Background(), fixture.state, identity, fixture.deployment); err != nil {
		t.Fatalf("accepted state-only serve = %v", err)
	}

	unmounted := fixture.data + "-unmounted"
	if err := os.Rename(fixture.data, unmounted); err != nil {
		t.Fatal(err)
	}
	if err := StateOnlyServingAllowed(context.Background(), fixture.state, identity, fixture.deployment); err != nil {
		t.Fatalf("state-only serve required a data mount: %v", err)
	}
	if err := fixture.serving(testBCommit, testBRelease); !IsKind(err, ErrorConflict) {
		t.Fatalf("data-opening serve without data = %v", err)
	}
	if err := os.Rename(unmounted, fixture.data); err != nil {
		t.Fatal(err)
	}

	tamper := filepath.Join(paths(fixture.state).rollback, "tamper")
	writePrivateFile(t, tamper, []byte("tamper"))
	if err := StateOnlyServingAllowed(context.Background(), fixture.state, identity, fixture.deployment); !IsKind(err, ErrorVerification) {
		t.Fatalf("state-only serve with changed rollback tree = %v", err)
	}
	if err := os.Remove(tamper); err != nil {
		t.Fatal(err)
	}
	if err := StateOnlyServingAllowed(context.Background(), fixture.state, identity, fixture.deployment); err != nil {
		t.Fatalf("restored state-only serve = %v", err)
	}
	if err := StateOnlyServingAllowed(
		context.Background(), fixture.state,
		deployment.Identity{ProjectCommit: testACommit, Release: testARelease}, fixture.deployment,
	); !IsKind(err, ErrorConflict) {
		t.Fatalf("state-only serve with wrong active identity = %v", err)
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
		if err := fixture.serving(testACommit, testARelease); !IsKind(err, ErrorInvalidState) {
			t.Fatalf("hard-linked state serve = %v", err)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		fixture := newManagerFixture(t)
		if _, err := fixture.manager(testACommit, testARelease).Prepare(testBCommit, testBRelease); err != nil {
			t.Fatal(err)
		}
		writePrivateFile(t, paths(fixture.state).prepare, bytes.Repeat([]byte("x"), MaxStateBytes+1))
		if err := fixture.serving(testACommit, testARelease); !IsKind(err, ErrorInvalidState) {
			t.Fatalf("oversized state serve = %v", err)
		}
	})
}

func newManagerFixture(t *testing.T) managerFixture {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "state")
	data := filepath.Join(root, "data")
	restore := filepath.Join(root, "restore")
	for _, directory := range []string{state, data, restore} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	active := deployment.Manager{
		StateDir: state, DataDir: data,
		ProjectCommit: testACommit, Release: testARelease,
		Now: func() time.Time { return time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC) },
	}
	if _, err := active.Init(); err != nil {
		t.Fatal(err)
	}
	createContractDatabase(t, filepath.Join(data, "db.sqlite3"))
	writePrivateFile(t, filepath.Join(data, "rsa_key.pem"), []byte("private"))
	writePrivateFile(t, filepath.Join(data, "rsa_key.pub.pem"), []byte("public"))
	return managerFixture{
		state:   state,
		data:    data,
		restore: restore,
		deployment: config.Deployment{
			Root:       "/srv/vaultwarden-less",
			RuntimeUID: uint32(os.Geteuid()),
			RuntimeGID: uint32(os.Getegid()),
			ListenPort: 8080,
			DrillPort:  8081,
		},
		now: time.Date(2026, 8, 27, 12, 34, 56, 789_000_000, time.UTC),
	}
}

func (fixture managerFixture) serving(commit, release string) error {
	return ServingAllowed(
		context.Background(), fixture.state, fixture.data,
		deployment.Identity{ProjectCommit: commit, Release: release}, fixture.deployment,
	)
}

func (fixture managerFixture) manager(commit, release string) Manager {
	return Manager{
		StateDir: fixture.state, DataDir: fixture.data, RestoreRoot: fixture.restore, Deployment: fixture.deployment,
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
