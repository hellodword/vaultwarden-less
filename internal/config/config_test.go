package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const minimalBackupYAML = `upgrade_verification_targets:
  - primary
targets:
  - name: primary
    repository: rest:https://repository.example.test/vaultwarden
    options: []
notifiers: []
`

const targetlessBackupYAML = `upgrade_verification_targets: []
targets: []
notifiers: []
`

func TestBackupSchemaPreservesSemanticOrder(t *testing.T) {
	t.Parallel()
	document := `upgrade_verification_targets: [cold, primary]
targets:
  - name: cold
    repository: /srv/cold
    options:
      - name: first
        value: "1"
      - name: second
        value: ""
  - name: primary
    repository: b2:bucket:path
    options: []
notifiers: []
`
	cfg, err := DecodeBackup([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	if names := []string{cfg.Targets[0].Name, cfg.Targets[1].Name}; !slices.Equal(names, []string{"cold", "primary"}) {
		t.Fatalf("target order = %#v", names)
	}
	if !slices.Equal(cfg.UpgradeVerificationTargets, []string{"cold", "primary"}) {
		t.Fatalf("verification order = %#v", cfg.UpgradeVerificationTargets)
	}
	if got := cfg.Targets[0].Options; len(got) != 2 || got[0].Name != "first" || got[1].Name != "second" {
		t.Fatalf("option order = %#v", got)
	}
}

func TestBackupRejectsEveryStrictYAMLAndShapeViolation(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"implicit null targets":  strings.Replace(minimalBackupYAML, "  - name: primary\n    repository: rest:https://repository.example.test/vaultwarden\n    options: []\n", "", 1),
		"missing targets":        strings.Replace(minimalBackupYAML, "targets:\n  - name: primary\n    repository: rest:https://repository.example.test/vaultwarden\n    options: []\n", "", 1),
		"missing verification":   strings.Replace(minimalBackupYAML, "upgrade_verification_targets:\n  - primary\n", "", 1),
		"empty verification":     strings.Replace(minimalBackupYAML, "upgrade_verification_targets:\n  - primary", "upgrade_verification_targets: []", 1),
		"unknown verification":   strings.Replace(minimalBackupYAML, "  - primary\n", "  - missing\n", 1),
		"missing options":        strings.Replace(minimalBackupYAML, "    options: []\n", "", 1),
		"missing notifiers":      strings.Replace(minimalBackupYAML, "notifiers: []\n", "", 1),
		"unknown field":          minimalBackupYAML + "state_dir: /elsewhere\n",
		"duplicate key":          minimalBackupYAML + "notifiers: []\n",
		"multiple documents":     minimalBackupYAML + "---\nnotifiers: []\n",
		"boolean string":         strings.Replace(minimalBackupYAML, "repository: rest:", "repository: true # rest:", 1),
		"numeric option value":   strings.Replace(minimalBackupYAML, "options: []", "options: [{name: connections, value: 3}]", 1),
		"anchor":                 strings.Replace(minimalBackupYAML, "repository: rest:", "repository: &repository rest:", 1),
		"alias":                  strings.Replace(minimalBackupYAML, "notifiers: []", "notifiers: *repository", 1),
		"tag":                    strings.Replace(minimalBackupYAML, "repository: rest:", "repository: !!str rest:", 1),
		"merge":                  minimalBackupYAML + "extra: &extra {name: x}\nmerge: {<<: *extra}\n",
		"duplicate option":       strings.Replace(minimalBackupYAML, "options: []", "options: [{name: x, value: a}, {name: x, value: b}]", 1),
		"option equals":          strings.Replace(minimalBackupYAML, "options: []", "options: [{name: x=y, value: a}]", 1),
		"repository newline":     strings.Replace(minimalBackupYAML, "repository: rest:https://repository.example.test/vaultwarden", "repository: |\n      bad\n      value", 1),
		"duplicate target":       strings.Replace(minimalBackupYAML, "notifiers: []", "  - name: primary\n    repository: other\n    options: []\nnotifiers: []", 1),
		"duplicate verification": strings.Replace(minimalBackupYAML, "  - primary\n", "  - primary\n  - primary\n", 1),
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeBackup([]byte(document)); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}

func TestTargetlessBackupRequiresExplicitEmptyPrivateMaterial(t *testing.T) {
	t.Parallel()
	cfg, err := DecodeBackup([]byte(targetlessBackupYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Targets == nil || len(cfg.Targets) != 0 || cfg.UpgradeVerificationTargets == nil || len(cfg.UpgradeVerificationTargets) != 0 {
		t.Fatalf("targetless config = %#v", cfg)
	}
	credentials, err := LoadRoleCredentials(writePrivateFile(t, "targets: {}\n"), cfg)
	if err != nil || credentials.Targets == nil || len(credentials.Targets) != 0 {
		t.Fatalf("targetless role credentials = %#v/%v", credentials, err)
	}
	notifiers, err := LoadNotifierCredentials(writePrivateFile(t, "notifiers: {}\n"), cfg)
	if err != nil || notifiers == nil || len(notifiers) != 0 {
		t.Fatalf("targetless notifier credentials = %#v/%v", notifiers, err)
	}
	if err := ValidatePasswordDirectory(privateDirectory(t), cfg); err != nil {
		t.Fatalf("empty password directory = %v", err)
	}

	for name, document := range map[string]string{
		"missing targets":       "upgrade_verification_targets: []\nnotifiers: []\n",
		"missing verification":  "targets: []\nnotifiers: []\n",
		"nonempty verification": "upgrade_verification_targets:\n  - primary\ntargets: []\nnotifiers: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeBackup([]byte(document)); err == nil {
				t.Fatal("invalid targetless configuration was accepted")
			}
		})
	}
}

func TestYAMLErrorsDoNotEchoSecretValues(t *testing.T) {
	t.Parallel()
	secret := "must-not-appear-in-errors"
	_, err := DecodeBackup([]byte(minimalBackupYAML + "unexpected: " + secret + "\n"))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe decode error: %v", err)
	}
}

func TestRoleCredentialsRequireExactTargetsAndCleanEnvironment(t *testing.T) {
	t.Parallel()
	cfg, err := DecodeBackup([]byte(minimalBackupYAML))
	if err != nil {
		t.Fatal(err)
	}
	valid := `targets:
  primary:
    environment:
      AWS_ACCESS_KEY_ID: synthetic-access
      AWS_SECRET_ACCESS_KEY: synthetic-secret
      MULTILINE: |-
        line one
        line two
`
	credentials, err := LoadRoleCredentials(writePrivateFile(t, valid), cfg)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := credentials.Environment(cfg.Targets[0])
	if err != nil || environment["MULTILINE"] != "line one\nline two" {
		t.Fatalf("environment = %#v/%v", environment, err)
	}
	environment["AWS_ACCESS_KEY_ID"] = "changed"
	again, _ := credentials.Environment(cfg.Targets[0])
	if again["AWS_ACCESS_KEY_ID"] != "synthetic-access" {
		t.Fatal("environment map escaped by reference")
	}

	for name, invalid := range map[string]string{
		"extra target":    valid + "  extra:\n    environment: {}\n",
		"unknown field":   strings.Replace(valid, "    environment:", "    other: value\n    environment:", 1),
		"reserved restic": strings.Replace(valid, "      AWS_ACCESS_KEY_ID:", "      RESTIC_PASSWORD: forbidden\n      AWS_ACCESS_KEY_ID:", 1),
		"reserved home":   strings.Replace(valid, "      AWS_ACCESS_KEY_ID:", "      HOME: forbidden\n      AWS_ACCESS_KEY_ID:", 1),
		"bad name":        strings.Replace(valid, "      AWS_ACCESS_KEY_ID:", "      lowercase: forbidden\n      AWS_ACCESS_KEY_ID:", 1),
		"empty value":     strings.Replace(valid, "synthetic-access", `""`, 1),
		"implicit value":  strings.Replace(valid, "synthetic-access", "123", 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := LoadRoleCredentials(writePrivateFile(t, invalid), cfg); err == nil {
				t.Fatal("invalid role credentials were accepted")
			}
		})
	}
}

func TestNotifierFieldsAndBarkKeyAreStrict(t *testing.T) {
	t.Parallel()
	backup := strings.Replace(minimalBackupYAML, "notifiers: []", `notifiers:
  - name: operations
    type: wechat
  - name: phone
    type: bark`, 1)
	cfg, err := DecodeBackup([]byte(backup))
	if err != nil {
		t.Fatal(err)
	}
	key := p256PKCS8(t)
	validCredentials := `notifiers:
  operations:
    webhook_url: https://example.test/hooks/send?key=placeholder
  phone:
    device_token: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    auth_key: |
` + indent(key, 6) + `    key_id: KEY123
    team_id: TEAM123
    topic: me.example.app
`
	resolved, err := LoadNotifierCredentials(writePrivateFile(t, validCredentials), cfg)
	if err != nil || len(resolved) != 2 || resolved[0].WebhookURL == "" || resolved[1].Sound != "1107" {
		t.Fatalf("resolved notifier credentials = %#v/%v", resolved, err)
	}
	for name, credentials := range map[string]string{
		"http webhook":   strings.Replace(validCredentials, "https://", "http://", 1),
		"wrong key":      strings.Replace(validCredentials, "-----BEGIN PRIVATE KEY-----", "not-a-key", 1),
		"trailing PEM":   strings.Replace(validCredentials, "    key_id:", "      extra\n    key_id:", 1),
		"wechat extra":   strings.Replace(validCredentials, "    webhook_url:", "    sound: chime\n    webhook_url:", 1),
		"bark missing":   strings.Replace(validCredentials, "    topic: me.example.app\n", "", 1),
		"extra notifier": validCredentials + "  unexpected:\n    webhook_url: https://example.test/hook\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := LoadNotifierCredentials(writePrivateFile(t, credentials), cfg); err == nil {
				t.Fatal("invalid notifier credentials were accepted")
			}
		})
	}
}

func TestEmptyNotifierSetStillRequiresStrictCredentialFile(t *testing.T) {
	t.Parallel()
	cfg, err := DecodeBackup([]byte(minimalBackupYAML))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := LoadNotifierCredentials(writePrivateFile(t, "notifiers: {}\n"), cfg)
	if err != nil || resolved == nil || len(resolved) != 0 {
		t.Fatalf("empty notifier credentials = %#v/%v", resolved, err)
	}
	if _, err := LoadNotifierCredentials(writePrivateFile(t, "notifiers: {}\nunknown: value\n"), cfg); err == nil {
		t.Fatal("invalid empty notifier credential file was accepted")
	}
}

func TestPasswordDirectoryRequiresExactSetAndOnePrivateLine(t *testing.T) {
	t.Parallel()
	cfg, err := DecodeBackup([]byte(minimalBackupYAML))
	if err != nil {
		t.Fatal(err)
	}
	directory := privateDirectory(t)
	writeModeFile(t, filepath.Join(directory, "primary.example"), "template\n", 0o644)
	writeModeFile(t, filepath.Join(directory, "primary"), "password\n", 0o600)
	if err := ValidatePasswordDirectory(directory, cfg); err != nil {
		t.Fatal(err)
	}
	if password, err := ReadPassword(directory, "primary"); err != nil || password != "password" {
		t.Fatalf("password = %q/%v", password, err)
	}
	for name, value := range map[string]string{"empty": "", "two lines": "one\ntwo", "CR": "one\r\n", "NUL": "one\x00"} {
		t.Run(name, func(t *testing.T) {
			directory := privateDirectory(t)
			writeModeFile(t, filepath.Join(directory, "primary"), value, 0o600)
			if err := ValidatePasswordDirectory(directory, cfg); err == nil {
				t.Fatal("invalid password was accepted")
			}
		})
	}
	writeModeFile(t, filepath.Join(directory, "unexpected"), "password", 0o600)
	if err := ValidatePasswordDirectory(directory, cfg); err == nil {
		t.Fatal("unexpected password file was accepted")
	}
}

func TestPrivateConfigFileAndFixedPolicy(t *testing.T) {
	t.Parallel()
	directory := privateDirectory(t)
	path := filepath.Join(directory, "backup.yaml")
	writeModeFile(t, path, minimalBackupYAML, 0o600)
	if _, err := LoadBackup(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBackup(path); err == nil {
		t.Fatal("public config file was accepted")
	}
	if QuietWindow != 30*time.Second || MaximumQuietWindow != 2*time.Minute || MinimumCheckInterval != 5*time.Minute ||
		FallbackCheckInterval != 12*time.Hour || ConsistencyRetry != 30*time.Second || FailureRetry != 5*time.Minute ||
		DegradedAfter != 30*time.Minute || EventRetry != 30*time.Second || TargetTimeout != 2*time.Hour ||
		MaintenanceTimeout != 24*time.Hour || NotifierTimeout != 10*time.Second || ShutdownTimeout != 2*time.Minute {
		t.Fatal("fixed runtime policy changed")
	}
}

func p256PKCS8(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func indent(value string, spaces int) string {
	prefix := strings.Repeat(" ", spaces)
	value = strings.TrimSuffix(value, "\n")
	return prefix + strings.ReplaceAll(value, "\n", "\n"+prefix) + "\n"
}

func privateDirectory(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func writePrivateFile(t *testing.T, contents string) string {
	t.Helper()
	directory := privateDirectory(t)
	path := filepath.Join(directory, "private.yaml")
	writeModeFile(t, path, contents, 0o600)
	return path
}

func writeModeFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}
