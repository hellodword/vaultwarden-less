package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

type composeGateService struct {
	Command     []string `json:"command"`
	NetworkMode string   `json:"network_mode"`
	Volumes     []struct {
		Type     string `json:"type"`
		Source   string `json:"source"`
		Target   string `json:"target"`
		ReadOnly bool   `json:"read_only"`
	} `json:"volumes"`
}

func TestComposeStartupGateModel(t *testing.T) {
	repository := repositoryRoot(t)
	root := t.TempDir()
	for _, relative := range []string{"config", "data", "restore", "restore/drill", "state"} {
		if err := os.MkdirAll(filepath.Join(root, relative), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, relative), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	deploymentEnvironment := fmt.Sprintf(
		"DEPLOYMENT_SCHEMA_VERSION=1\nRUNTIME_UID=%d\nRUNTIME_GID=%d\nLISTEN_PORT=18080\nDRILL_PORT=18081\n",
		os.Geteuid(), os.Getegid(),
	)
	writePrivateFile(t, filepath.Join(root, ".env"), deploymentEnvironment)
	writePrivateFile(t, filepath.Join(root, "config", "vaultwarden.env"), "DOMAIN=https://vaultwarden.example.test\n")

	base := renderComposeModel(t, root, filepath.Join(root, ".env"),
		filepath.Join(repository, "compose.yml"), "--profile", "deployment", "--profile", "drill")
	assertGateCommand(t, base["vaultwarden"], "normal")
	assertGateMount(t, base["vaultwarden"], filepath.Join(root, ".env"), "/deployment/.env", true)
	assertGateMount(t, base["vaultwarden"], filepath.Join(root, "state"), "/state", true)
	assertGateMount(t, base["vaultwarden"], filepath.Join(root, "data"), "/data", false)

	assertGateCommand(t, base["drill-vaultwarden"], "drill")
	assertGateMount(t, base["drill-vaultwarden"], filepath.Join(root, ".env"), "/deployment/.env", true)
	assertGateMount(t, base["drill-vaultwarden"], filepath.Join(root, "state"), "/state", true)
	assertGateMount(t, base["drill-vaultwarden"], filepath.Join(root, "restore", "drill"), "/recovery", true)

	admin, exists := base["deployment-admin"]
	if !exists || admin.NetworkMode != "none" || len(admin.Volumes) != 3 {
		t.Fatalf("deployment-admin isolation = exists:%t network:%q volumes:%#v", exists, admin.NetworkMode, admin.Volumes)
	}
	assertGateMount(t, admin, filepath.Join(root, ".env"), "/deployment/.env", true)
	assertGateMount(t, admin, filepath.Join(root, "data"), "/data", true)
	assertGateMount(t, admin, filepath.Join(root, "state"), "/state", false)

	upgradeEnvironment := filepath.Join(root, "compose.env")
	writePrivateFile(t, upgradeEnvironment, deploymentEnvironment+
		"A_RELEASE=RELEASE.2026-08-28T12-00-00Z-gaaaaaaaaaaaa\n"+
		"B_RELEASE=RELEASE.2026-08-28T13-00-00Z-gbbbbbbbbbbbb\n")
	upgradeModel := renderComposeModel(
		t,
		root,
		upgradeEnvironment,
		filepath.Join(repository, "compose.yml"),
		"--file", filepath.Join(repository, "compose.upgrade.yml"),
		"--profile", "rollback",
	)
	assertGateCommand(t, upgradeModel["vaultwarden"], "upgrade-candidate")
	assertGateCommand(t, upgradeModel["rollback-vaultwarden-a"], "rollback")
	assertGateMount(t, upgradeModel["rollback-vaultwarden-a"], filepath.Join(root, ".env"), "/deployment/.env", true)
	assertGateMount(t, upgradeModel["rollback-vaultwarden-a"], filepath.Join(root, "state"), "/state", true)
}

func renderComposeModel(t *testing.T, root, environment, compose string, extra ...string) map[string]composeGateService {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	arguments := []string{
		"compose", "--env-file", environment,
		"--project-directory", root,
		"--project-name", "vwless-gate-model-" + strconv.Itoa(os.Getpid()),
		"--file", compose,
	}
	arguments = append(arguments, extra...)
	arguments = append(arguments, "config", "--format", "json")
	command := exec.CommandContext(ctx, "docker", arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("render Compose gate model: %v: %s", err, output)
	}
	var model struct {
		Services map[string]composeGateService `json:"services"`
	}
	if err := json.Unmarshal(output, &model); err != nil {
		t.Fatalf("decode Compose gate model: %v", err)
	}
	return model.Services
}

func assertGateCommand(t *testing.T, service composeGateService, mode string) {
	t.Helper()
	if !slices.Equal(service.Command, []string{mode}) {
		t.Fatalf("gate command = %#v, want [%s]", service.Command, mode)
	}
}

func assertGateMount(t *testing.T, service composeGateService, source, target string, readOnly bool) {
	t.Helper()
	for _, volume := range service.Volumes {
		if volume.Type == "bind" && filepath.Clean(volume.Source) == filepath.Clean(source) &&
			volume.Target == target && volume.ReadOnly == readOnly {
			return
		}
	}
	t.Fatalf("missing gate mount source=%q target=%q read_only=%t in %#v", source, target, readOnly, service.Volumes)
}
