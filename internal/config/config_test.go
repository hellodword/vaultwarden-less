package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalBackupYAML = `schema_version: 1
target_timeout: 30m
maintenance_timeout: 12h
`

func TestTrackedBackupTemplateValidates(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "backup.template.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := DecodeBackup(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Targets) != 0 || len(cfg.Notifiers) != 0 {
		t.Fatalf("template should default to no external integrations: %#v", cfg)
	}
}

func TestBackupAllowsOmittedOrEmptyTargets(t *testing.T) {
	t.Parallel()
	for _, suffix := range []string{"", "targets: []\n"} {
		cfg, err := DecodeBackup([]byte(minimalBackupYAML + suffix))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Targets == nil || len(cfg.Targets) != 0 {
			t.Fatalf("targets = %#v", cfg.Targets)
		}
	}
}

func TestBackupRejectsUnknownFieldsMultipleDocumentsAndUnsafeRepositories(t *testing.T) {
	t.Parallel()
	for name, document := range map[string]string{
		"unknown field":      minimalBackupYAML + "state_dir: /elsewhere\n",
		"duplicate key":      minimalBackupYAML + "target_timeout: 20m\n",
		"multiple documents": minimalBackupYAML + "---\nschema_version: 1\n",
		"local repository":   minimalBackupYAML + "targets:\n  - name: local\n    repository: /srv/restic\n",
		"REST repository":    minimalBackupYAML + "targets:\n  - name: rest\n    repository: rest:https://example.test/repo\n",
		"S3 user info":       minimalBackupYAML + "targets:\n  - name: s3\n    repository: s3:https://user:secret@example.test/bucket/repo\n",
		"S3 query":           minimalBackupYAML + "targets:\n  - name: s3\n    repository: s3:https://example.test/bucket/repo?secret=value\n",
		"S3 no bucket":       minimalBackupYAML + "targets:\n  - name: s3\n    repository: s3:https://example.test\n",
		"B2 no prefix":       minimalBackupYAML + "targets:\n  - name: b2\n    repository: b2:bucket-name:\n",
		"B2 unsafe path":     minimalBackupYAML + "targets:\n  - name: b2\n    repository: b2:bucket-name:../repository\n",
	} {
		document := document
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := DecodeBackup([]byte(document)); err == nil {
				t.Fatal("invalid configuration was accepted")
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

func TestDurationRejectsNonStringAndNonPositiveValues(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"0s", "-1s", "1"} {
		document := "schema_version: 1\ntarget_timeout: " + value + "\nmaintenance_timeout: 12h\n"
		if _, err := DecodeBackup([]byte(document)); err == nil {
			t.Fatalf("accepted invalid duration %q", value)
		}
	}
}

func TestTargetsSupportOnlyS3AndNativeB2(t *testing.T) {
	t.Parallel()
	document := minimalBackupYAML + `targets:
  - name: archive-s3
    repository: s3:https://objects.example.test/vaultwarden/restic
    restic_options:
      s3.bucket-lookup: path
      s3.region: ap-singapore
  - name: archive-b2
    repository: b2:bucket-name:vaultwarden/restic
`
	cfg, err := DecodeBackup([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Targets[0].Backend() != "s3" || cfg.Targets[1].Backend() != "b2" {
		t.Fatalf("backends = %q/%q", cfg.Targets[0].Backend(), cfg.Targets[1].Backend())
	}
}

func TestS3OptionsUseNarrowTypedValues(t *testing.T) {
	t.Parallel()
	for name, option := range map[string]string{
		"bucket lookup": "s3.bucket-lookup: command injection",
		"list v1":       "s3.list-objects-v1: sometimes",
		"region":        "s3.region: region with spaces",
		"unknown":       "s3.connections: '99'",
	} {
		option := option
		t.Run(name, func(t *testing.T) {
			document := minimalBackupYAML + "targets:\n  - name: primary\n    repository: s3:https://objects.example.test/bucket/repository\n    restic_options:\n      " + option + "\n"
			if _, err := DecodeBackup([]byte(document)); err == nil {
				t.Fatal("invalid S3 option was accepted")
			}
		})
	}
}

func TestNotifierContractContainsOnlyWechatAndBark(t *testing.T) {
	t.Parallel()
	document := minimalBackupYAML + `notifiers:
  - name: operations
    type: wechat
    timeout: 10s
    webhook_url: https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=placeholder
  - name: phone
    type: bark
    timeout: 10s
    device_token: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    auth_key: |
      -----BEGIN PRIVATE KEY-----
      placeholder
      -----END PRIVATE KEY-----
    key_id: KEY123
    team_id: TEAM123
    topic: me.fin.bark
`
	if _, err := DecodeBackup([]byte(document)); err != nil {
		t.Fatal(err)
	}
	unsupported := minimalBackupYAML + "notifiers:\n  - name: generic\n    type: webhook\n    timeout: 10s\n    webhook_url: https://example.test/hook\n"
	if _, err := DecodeBackup([]byte(unsupported)); err == nil {
		t.Fatal("generic webhook was accepted")
	}
}

func TestRoleCredentialsRequireOnlyConfiguredBackendKeys(t *testing.T) {
	t.Parallel()
	cfg, err := DecodeBackup([]byte(minimalBackupYAML + `targets:
  - name: primary
    repository: s3:https://objects.example.test/bucket/repository
  - name: cold
    repository: b2:cold-bucket:repository
`))
	if err != nil {
		t.Fatal(err)
	}
	path := writePrivateFile(t, `schema_version: 1
targets:
  primary:
    AWS_ACCESS_KEY_ID: synthetic-access
    AWS_SECRET_ACCESS_KEY: synthetic-secret
    IGNORED_SESSION_TOKEN: ignored
  cold:
    B2_ACCOUNT_ID: synthetic-id
    B2_ACCOUNT_KEY: synthetic-key
  removed-target:
    arbitrary: ignored
unknown_root_field: ignored
`)
	credentials, err := LoadRoleCredentials(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := credentials.Environment(cfg.Targets[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(environment) != 2 || environment["AWS_ACCESS_KEY_ID"] != "synthetic-access" {
		t.Fatalf("environment = %#v", environment)
	}
}

func TestRoleCredentialsRejectMissingRequiredKeyAndUnsafeFileMode(t *testing.T) {
	t.Parallel()
	cfg, err := DecodeBackup([]byte(minimalBackupYAML + "targets:\n  - name: primary\n    repository: s3:https://objects.example.test/bucket/repository\n"))
	if err != nil {
		t.Fatal(err)
	}
	path := writePrivateFile(t, "schema_version: 1\ntargets:\n  primary:\n    AWS_ACCESS_KEY_ID: present\n")
	if _, err := LoadRoleCredentials(path, cfg); err == nil {
		t.Fatal("missing secret access key was accepted")
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRoleCredentials(path, cfg); err == nil {
		t.Fatal("group-readable credentials were accepted")
	}
}

func TestFixedRuntimeDefaultsAreNotYAMLFields(t *testing.T) {
	t.Parallel()
	cfg, err := DecodeBackup([]byte(minimalBackupYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != DataDir || cfg.StateDir != StateDir || cfg.Debounce.Duration != 30*time.Second {
		t.Fatalf("fixed defaults not populated: %#v", cfg)
	}
	if _, err := DecodeBackup([]byte(minimalBackupYAML + "data_dir: /other\n")); err == nil {
		t.Fatal("fixed runtime path was accepted from YAML")
	}
}

func writePrivateFile(t *testing.T, contents string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "credentials")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "private.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
