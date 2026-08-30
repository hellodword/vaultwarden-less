package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hellodword/vaultwarden-less/internal/buildinfo"
	"github.com/hellodword/vaultwarden-less/internal/config"
	"github.com/hellodword/vaultwarden-less/internal/deployment"
	"github.com/hellodword/vaultwarden-less/internal/importer"
	"github.com/hellodword/vaultwarden-less/internal/privatefs"
	"github.com/hellodword/vaultwarden-less/internal/upgrade"
)

const (
	vaultwardenBinary = "/bin/vaultwarden"
	recoveryRoot      = "/recovery"
	manifestMaxBytes  = 1 << 20

	modeNormal           = "normal"
	modeUpgradeCandidate = "upgrade-candidate"
	modeRollback         = "rollback"
	modeDrill            = "drill"
	modeImport           = "import"
)

type drillManifest struct {
	ProjectCommit string            `json:"project_commit"`
	ContentID     string            `json:"content_id"`
	ArchiveID     string            `json:"archive_id"`
	Files         []json.RawMessage `json:"files"`
}

type gateEnvironment struct {
	ready             func() bool
	identity          func() deployment.Identity
	loadDeployment    func(string) (config.Deployment, error)
	requireActive     func(string, deployment.Identity) (deployment.Active, error)
	readActive        func(string) (deployment.Active, error)
	allowNormal       func(context.Context, string, string, deployment.Identity, config.Deployment) error
	allowCandidate    func(string, string, string, string, string, config.Deployment) error
	readDrillManifest func(context.Context, string) (drillManifest, error)
	allowDrill        func(string, string, string, string, string, config.Deployment) error
	allowRollback     func(context.Context, string, string, deployment.Identity, config.Deployment) error
	allowImport       func(context.Context, string, string, string, deployment.Identity, config.Deployment) error
	start             func() error
	deploymentPath    string
	stateDir          string
	dataDir           string
	importSourceDir   string
	recoveryRoot      string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := execute(ctx, os.Args[1:], os.Stderr, defaultGateEnvironment())
	stop()
	os.Exit(code)
}

func defaultGateEnvironment() gateEnvironment {
	return gateEnvironment{
		ready: buildinfo.Ready,
		identity: func() deployment.Identity {
			return deployment.Identity{ProjectCommit: buildinfo.ProjectCommit, Release: buildinfo.Release}
		},
		loadDeployment: config.LoadDeployment,
		requireActive:  deployment.RequireActive,
		readActive:     deployment.ReadActive,
		allowNormal: func(ctx context.Context, stateDir, dataDir string, identity deployment.Identity, configured config.Deployment) error {
			if err := importer.NormalServingAllowed(stateDir, identity); err != nil {
				return err
			}
			return upgrade.ServingAllowed(ctx, stateDir, dataDir, identity, configured)
		},
		allowCandidate: upgrade.CandidateServingAllowed,
		readDrillManifest: func(ctx context.Context, root string) (drillManifest, error) {
			return readDrillManifest(ctx, root)
		},
		allowDrill:    upgrade.DrillServingAllowed,
		allowRollback: upgrade.RollbackServingAllowed,
		allowImport:   importer.CandidateServingAllowed,
		start: func() error {
			return syscall.Exec(vaultwardenBinary, []string{vaultwardenBinary}, os.Environ())
		},
		deploymentPath:  config.DeploymentEnvPath,
		stateDir:        config.StateDir,
		dataDir:         config.DataDir,
		importSourceDir: filepath.Join(config.ImportRoot, "source"),
		recoveryRoot:    recoveryRoot,
	}
}

func execute(ctx context.Context, arguments []string, stderr io.Writer, environment gateEnvironment) int {
	if len(arguments) != 1 || !validMode(arguments[0]) {
		_, _ = fmt.Fprintln(stderr, "vaultwarden-less-vaultwarden requires exactly one supported gate mode")
		return 2
	}
	if environment.ready == nil || !environment.ready() || environment.identity == nil {
		_, _ = fmt.Fprintln(stderr, "vaultwarden startup gate rejected the requested mode")
		return 1
	}
	identity := environment.identity()
	if err := identity.Validate(); err != nil {
		_, _ = fmt.Fprintln(stderr, "vaultwarden startup gate rejected the requested mode")
		return 1
	}
	if err := evaluateGate(ctx, arguments[0], identity, environment); err != nil {
		_, _ = fmt.Fprintln(stderr, "vaultwarden startup gate rejected the requested mode")
		return 1
	}
	if environment.start == nil || environment.start() != nil {
		_, _ = fmt.Fprintln(stderr, "vaultwarden process could not be started")
		return 1
	}
	return 0
}

func evaluateGate(ctx context.Context, mode string, identity deployment.Identity, environment gateEnvironment) error {
	if environment.loadDeployment == nil {
		return fmt.Errorf("deployment loader is unavailable")
	}
	configured, err := environment.loadDeployment(environment.deploymentPath)
	if err != nil {
		return err
	}
	switch mode {
	case modeNormal:
		if environment.requireActive == nil || environment.allowNormal == nil {
			return fmt.Errorf("normal gate is unavailable")
		}
		if _, err := environment.requireActive(environment.stateDir, identity); err != nil {
			return err
		}
		return environment.allowNormal(ctx, environment.stateDir, environment.dataDir, identity, configured)
	case modeUpgradeCandidate:
		if environment.readActive == nil || environment.allowCandidate == nil {
			return fmt.Errorf("candidate gate is unavailable")
		}
		active, err := environment.readActive(environment.stateDir)
		if err != nil {
			return err
		}
		return environment.allowCandidate(
			environment.stateDir,
			identity.ProjectCommit,
			identity.Release,
			active.ProjectCommit,
			active.Release,
			configured,
		)
	case modeRollback:
		if environment.allowRollback == nil {
			return fmt.Errorf("rollback gate is unavailable")
		}
		return environment.allowRollback(ctx, environment.stateDir, environment.dataDir, identity, configured)
	case modeDrill:
		if environment.readDrillManifest == nil || environment.allowDrill == nil {
			return fmt.Errorf("drill gate is unavailable")
		}
		manifest, err := environment.readDrillManifest(ctx, environment.recoveryRoot)
		if err != nil {
			return err
		}
		if manifest.ProjectCommit != identity.ProjectCommit {
			return fmt.Errorf("drill manifest belongs to another project commit")
		}
		return environment.allowDrill(
			environment.stateDir,
			identity.ProjectCommit,
			identity.Release,
			manifest.ContentID,
			manifest.ArchiveID,
			configured,
		)
	case modeImport:
		if environment.allowImport == nil {
			return fmt.Errorf("import gate is unavailable")
		}
		return environment.allowImport(
			ctx,
			environment.stateDir,
			environment.importSourceDir,
			environment.dataDir,
			identity,
			configured,
		)
	default:
		return fmt.Errorf("unsupported gate mode")
	}
}

func validMode(value string) bool {
	switch value {
	case modeNormal, modeUpgradeCandidate, modeRollback, modeDrill, modeImport:
		return true
	default:
		return false
	}
}

func readDrillManifest(ctx context.Context, root string) (drillManifest, error) {
	if err := ctx.Err(); err != nil {
		return drillManifest{}, err
	}
	if err := privatefs.ValidatePrivateDirectory(root); err != nil {
		return drillManifest{}, fmt.Errorf("validate recovery root: %w", err)
	}
	data, err := privatefs.ReadBytes(filepath.Join(root, "manifest.json"), manifestMaxBytes)
	if err != nil {
		return drillManifest{}, fmt.Errorf("read recovery manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest drillManifest
	if err := decoder.Decode(&manifest); err != nil {
		return drillManifest{}, fmt.Errorf("decode recovery manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return drillManifest{}, fmt.Errorf("recovery manifest contains trailing values")
	}
	if !validHex(manifest.ProjectCommit, 40) ||
		!validDigest(manifest.ContentID) || !validDigest(manifest.ArchiveID) || len(manifest.Files) == 0 {
		return drillManifest{}, fmt.Errorf("recovery manifest identity is invalid")
	}
	return manifest, nil
}

func validDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validHex(strings.TrimPrefix(value, "sha256:"), 64)
}

func validHex(value string, size int) bool {
	if len(value) != size || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
