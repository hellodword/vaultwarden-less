package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIndependentConfigurationErrors(t *testing.T) {
	cfg, err := DecodeBackup([]byte(`targets:
  - name: broken
    env_file: /missing
    typo: secret-do-not-log
  - name: healthy
    env_file: /run/primary.env
notifiers:
  - name: bad
    type: wecom
    webhook_url: https://example.test/send
    secret: not-allowed
  - name: good
    type: feishu
    webhook_url: https://example.test/send
    secret: optional
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Targets[0].Error == "" || cfg.Targets[1].Error != "" || cfg.Notifiers[0].Error == "" || cfg.Notifiers[1].Error != "" {
		t.Fatalf("configuration isolation failed")
	}
	for _, data := range []string{"targets: []\nnotifiers: []\ntypo: 1", "[invalid", "targets: []\n---\ntargets: []"} {
		if _, err := DecodeBackup([]byte(data)); err == nil {
			t.Fatal("accepted invalid document")
		}
	}
	if cfg, err := DecodeBackup([]byte("targets: []\nnotifiers: []\n")); err != nil || !cfg.Valid() {
		t.Fatal("empty configuration invalid")
	}
}

func TestDockerLiteralEnvironmentAndNoInheritance(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("INHERITED_SECRET", "never inherit")
	secret := "RESTIC_REPOSITORY=/repo\nRESTIC_PASSWORD='literal $HOME # quote'\nINHERITED_SECRET\nAWS_ACCESS_KEY_ID=first\nAWS_ACCESS_KEY_ID=last\n"
	if err := os.WriteFile(filepath.Join(dir, "primary.env"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "second.env"), []byte("RESTIC_REPOSITORY=/second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	document := "targets:\n  - name: first\n    env_file: " + filepath.Join(dir, "primary.env") + "\n  - name: second\n    env_file: " + filepath.Join(dir, "second.env") + "\nnotifiers: []\n"
	path := filepath.Join(dir, "backup.yaml")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Valid() {
		t.Fatal("valid local env rejected")
	}
	got := strings.Join(cfg.Targets[0].Environment, "\n")
	if !strings.Contains(got, "RESTIC_PASSWORD='literal $HOME # quote'") || strings.Contains(got, "INHERITED_SECRET") {
		t.Fatal("environment semantics changed")
	}
	if len(cfg.Targets[1].Environment) != 1 {
		t.Fatal("target inherited credentials")
	}
	if err := os.WriteFile(filepath.Join(dir, "primary.env"), []byte("=sensitive-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Targets[0].Error != "invalid_env_file" || cfg.Targets[1].Error != "" {
		t.Fatal("env errors not isolated")
	}
}

func TestNamesAndSound(t *testing.T) {
	cfg, err := DecodeBackup([]byte(`targets:
  - name: 同名
    env_file: /one
  - name: 同名
    env_file: /two
notifiers:
  - name: bark
    type: bark
    device_token: varying-length
    auth_key_file: /test.p8
    key_id: key
    team_id: team
    topic: topic
    sound: ""
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Targets[0].Error != "duplicate_name" || cfg.Targets[1].Error != "duplicate_name" {
		t.Fatal("duplicate targets accepted")
	}
	if cfg.Notifiers[0].Error != "" || cfg.Notifiers[0].Sound != "" {
		t.Fatal("explicit sound or varying token length changed")
	}
}
