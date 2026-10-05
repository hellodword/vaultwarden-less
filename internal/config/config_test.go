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
		"aws-s3":        "us-east-1",
		"cloudflare-r2": "auto",
		"backblaze-b2":  "us-west-002",
	}
	data := strings.ReplaceAll(string(document), "/run/credentials/backup/bark-apns-auth-key.p8", keyPath)
	for name := range regions {
		envPath := filepath.Join(root, "credentials", "backup", name+"-backup.env")
		data = strings.ReplaceAll(data, "/run/credentials/backup/"+name+"-backup.env", envPath)
	}
	path := filepath.Join(t.TempDir(), "backup.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadBackup(path)
	if err != nil || !cfg.Valid() || len(cfg.Targets) != len(regions) || len(cfg.Notifiers) != 2 {
		t.Fatalf("invalid backup template: %v", err)
	}
	if cfg.Notifiers[0].Type != "wecom" || cfg.Notifiers[1].Type != "bark" || cfg.Notifiers[1].Key == nil {
		t.Fatal("expected working WeCom and Bark APNs examples after supplying a key")
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
			env["RESTIC_PASSWORD_FILE"] != "/run/credentials/restic/"+passwordFile ||
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
