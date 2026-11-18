package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
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
notifications:
 channels:
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
	if cfg.Targets[0].Error == "" || cfg.Targets[1].Error != "" || cfg.Notifications.Channels[0].Error == "" || cfg.Notifications.Channels[1].Error != "" {
		t.Fatalf("configuration isolation failed")
	}
	for _, data := range []string{"targets: []\nnotifications: {channels: []}\ntypo: 1", "[invalid", "targets: []\n---\ntargets: []"} {
		if _, err := DecodeBackup([]byte(data)); err == nil {
			t.Fatal("accepted invalid document")
		}
	}
	if cfg, err := DecodeBackup([]byte("targets: []\nnotifications: {channels: []}\n")); err != nil || !cfg.Valid() {
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
	if err := os.WriteFile(filepath.Join(dir, "second.env"), []byte("RESTIC_REPOSITORY=/second\nRESTIC_PASSWORD=test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	document := "targets:\n  - name: first\n    env_file: " + filepath.Join(dir, "primary.env") + "\n  - name: second\n    env_file: " + filepath.Join(dir, "second.env") + "\nnotifications: {channels: []}\n"
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
	if len(cfg.Targets[1].Environment) != 2 {
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
notifications:
 channels:
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
	if cfg.Notifications.Channels[0].Error != "" || cfg.Notifications.Channels[0].Sound != "" {
		t.Fatal("explicit sound or varying token length changed")
	}
}

func TestShippedBackupTemplatesUseConsumerSyntax(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "templates"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := os.ReadFile(filepath.Join(root, "config", "backup.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// The operator supplies this key; the templates must not ship a private key.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "bark-apns-auth-key.p8")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	regions := map[string]string{
		"aws-s3":       "us-east-1",
		"backblaze-b2": "us-west-002",
	}
	data := strings.ReplaceAll(string(document), "/run/credentials/backup/bark-apns-auth-key.p8", keyPath)
	for name := range regions {
		original := filepath.Join(root, "credentials", "backup", name+"-backup.env")
		contents, err := os.ReadFile(original)
		if err != nil {
			t.Fatal(err)
		}
		passwordPath := filepath.Join(t.TempDir(), name+"-repository-password.txt")
		os.WriteFile(passwordPath, []byte("synthetic-only"), 0o600)
		envPath := filepath.Join(t.TempDir(), name+"-backup.env")
		os.WriteFile(envPath, []byte(strings.ReplaceAll(string(contents), "/run/credentials/restic/"+name+"-repository-password.txt", passwordPath)), 0o600)
		data = strings.ReplaceAll(data, "/run/credentials/backup/"+name+"-backup.env", envPath)
	}
	path := filepath.Join(t.TempDir(), "backup.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadBackup(path)
	if err != nil || !cfg.Valid() || len(cfg.Targets) != len(regions) || len(cfg.Notifications.Channels) != 0 {
		t.Fatalf("invalid backup template: %v", err)
	}
	for _, target := range cfg.Targets {
		env := make(map[string]string)
		for _, entry := range target.Environment {
			name, value, _ := strings.Cut(entry, "=")
			env[name] = value
		}
		region, ok := regions[target.Name]
		passwordFile := target.Name + "-repository-password.txt"
		if !ok || len(env) != 5 || env["AWS_DEFAULT_REGION"] != region ||
			!strings.HasSuffix(env["RESTIC_PASSWORD_FILE"], "/"+passwordFile) ||
			!strings.HasPrefix(env["RESTIC_REPOSITORY"], "s3:https://") ||
			env["AWS_ACCESS_KEY_ID"] == "" || env["AWS_SECRET_ACCESS_KEY"] == "" {
			t.Fatalf("incorrect native restic inputs for %s", target.Name)
		}
		password, err := os.ReadFile(filepath.Join(root, "credentials", "restic", passwordFile))
		if err != nil || len(password) != 0 {
			t.Fatalf("%s must be an empty raw password placeholder", passwordFile)
		}
	}
}

func TestNotificationSchedule(t *testing.T) {
	for _, body := range []string{"timezone: Missing/Zone", "daily_at: '25:00'", "reminder_interval: 0h", "typo: value"} {
		if _, err := DecodeBackup([]byte("targets: []\nnotifications:\n  " + body + "\n")); err == nil {
			t.Fatal("invalid notification root accepted")
		}
	}
}
