package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const minimalBackupYAML = `schema_version: 2
targets:
  - name: primary
    repository: s3:https://objects.example.test/bucket/repository
`

func TestBackupSchemaPreservesTargetOrderAndRequiresTargets(t *testing.T) {
	t.Parallel()
	document := `schema_version: 2
targets:
  - name: cold
    repository: s3:https://objects.example.test/cold/restic
  - name: primary
    repository: s3:https://objects.example.test/primary/restic
`
	cfg, err := DecodeBackup([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{cfg.Targets[0].Name, cfg.Targets[1].Name}
	if !slices.Equal(names, []string{"cold", "primary"}) {
		t.Fatalf("target order = %#v", names)
	}
	for _, invalid := range []string{
		"schema_version: 2\ntargets: []\n",
		"schema_version: 2\n",
		minimalBackupYAML + "  - name: primary\n    repository: s3:https://objects.example.test/other/repository\n",
	} {
		if _, err := DecodeBackup([]byte(invalid)); err == nil {
			t.Fatal("invalid target list was accepted")
		}
	}
}

func TestBackupRejectsUnknownFieldsDurationsAndUnsafeRepositories(t *testing.T) {
	t.Parallel()
	for name, document := range map[string]string{
		"unknown field":      minimalBackupYAML + "state_dir: /elsewhere\n",
		"duration":           minimalBackupYAML + "target_timeout: 30m\n",
		"duplicate key":      minimalBackupYAML + "schema_version: 2\n",
		"multiple documents": minimalBackupYAML + "---\nschema_version: 2\n",
		"local repository":   "schema_version: 2\ntargets:\n  - name: local\n    repository: /srv/restic\n",
		"REST repository":    "schema_version: 2\ntargets:\n  - name: rest\n    repository: rest:https://example.test/repo\n",
		"S3 user info":       "schema_version: 2\ntargets:\n  - name: s3\n    repository: s3:https://user:secret@example.test/bucket/repo\n",
		"S3 query":           "schema_version: 2\ntargets:\n  - name: s3\n    repository: s3:https://example.test/bucket/repo?secret=value\n",
		"S3 no bucket":       "schema_version: 2\ntargets:\n  - name: s3\n    repository: s3:https://example.test\n",
		"S3 fragment":        "schema_version: 2\ntargets:\n  - name: s3\n    repository: s3:https://example.test/bucket/repo#fragment\n",
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

func TestHTTPRepositoryWarnsAndS3OptionsAreNarrow(t *testing.T) {
	t.Parallel()
	cfg, err := DecodeBackup([]byte("schema_version: 2\ntargets:\n  - name: primary\n    repository: s3:http://objects.example.test/bucket/repo\n"))
	if err != nil {
		t.Fatal(err)
	}
	if warnings := cfg.Targets[0].Warnings(); !slices.Equal(warnings, []string{"plaintext_s3_endpoint"}) {
		t.Fatalf("warnings = %#v", warnings)
	}
	for _, option := range []string{
		"s3.bucket-lookup: command injection",
		"s3.region: region with spaces",
		"s3.connections: '99'",
	} {
		document := minimalBackupYAML + "    restic_options:\n      " + option + "\n"
		if _, err := DecodeBackup([]byte(document)); err == nil {
			t.Fatalf("invalid S3 option was accepted: %s", option)
		}
	}
}

func TestNotifierFieldsAreStrictForEachType(t *testing.T) {
	t.Parallel()
	backup := minimalBackupYAML + `notifiers:
  - name: operations
    type: wechat
  - name: phone
    type: bark
`
	cfg, err := DecodeBackup([]byte(backup))
	if err != nil {
		t.Fatal(err)
	}
	validCredentials := `schema_version: 2
notifiers:
  operations:
    webhook_url: https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=placeholder
  phone:
    device_token: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    auth_key: |
      -----BEGIN PRIVATE KEY-----
      placeholder
      -----END PRIVATE KEY-----
    key_id: KEY123
    team_id: TEAM123
    topic: me.fin.bark
`
	resolved, err := LoadNotifierCredentials(writePrivateFile(t, validCredentials), cfg)
	if err != nil || len(resolved) != 2 || resolved[0].WebhookURL == "" || resolved[1].DeviceToken == "" {
		t.Fatalf("resolved notifier credentials = %#v/%v", resolved, err)
	}
	if _, err := DecodeBackup([]byte(minimalBackupYAML + "notifiers:\n  - name: operations\n    type: wechat\n    webhook_url: https://example.test/hook\n")); err == nil {
		t.Fatal("secret notifier field was accepted in backup.yaml")
	}
	if _, err := DecodeBackup([]byte(minimalBackupYAML + "notifiers:\n  - name: generic\n    type: webhook\n")); err == nil {
		t.Fatal("unsupported notifier type was accepted")
	}
	for _, credentials := range []string{
		strings.Replace(validCredentials, "https://qyapi.weixin.qq.com", "http://example.test", 1),
		strings.Replace(validCredentials, "  phone:\n", "  unexpected:\n", 1),
		validCredentials + "unknown_root_field: forbidden\n",
	} {
		if _, err := LoadNotifierCredentials(writePrivateFile(t, credentials), cfg); err == nil {
			t.Fatal("invalid notifier credentials were accepted")
		}
	}
	emptyCfg, err := DecodeBackup([]byte(minimalBackupYAML))
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := LoadNotifierCredentials("/not-mounted", emptyCfg); err != nil || len(resolved) != 0 {
		t.Fatalf("empty notifier list required credentials: %#v/%v", resolved, err)
	}
}

func TestRoleCredentialsRequireExactTargetsAndFields(t *testing.T) {
	t.Parallel()
	cfg, err := DecodeBackup([]byte(minimalBackupYAML))
	if err != nil {
		t.Fatal(err)
	}
	valid := `schema_version: 2
targets:
  primary:
    AWS_ACCESS_KEY_ID: synthetic-access
    AWS_SECRET_ACCESS_KEY: synthetic-secret
`
	credentials, err := LoadRoleCredentials(writePrivateFile(t, valid), cfg)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := credentials.Environment(cfg.Targets[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(environment) != 2 || environment["AWS_ACCESS_KEY_ID"] != "synthetic-access" || environment["AWS_SECRET_ACCESS_KEY"] != "synthetic-secret" {
		t.Fatalf("environment = %#v", environment)
	}

	for _, invalid := range []string{
		`schema_version: 2
targets:
  primary:
    AWS_ACCESS_KEY_ID: synthetic-access
`,
		valid + "  removed-target:\n    AWS_ACCESS_KEY_ID: other\n    AWS_SECRET_ACCESS_KEY: other\n",
		`schema_version: 2
targets:
  primary:
    AWS_ACCESS_KEY_ID: synthetic-access
    AWS_SECRET_ACCESS_KEY: synthetic-secret
    AWS_SESSION_TOKEN: forbidden
`,
		valid + "unknown_root_field: forbidden\n",
	} {
		if _, err := LoadRoleCredentials(writePrivateFile(t, invalid), cfg); err == nil {
			t.Fatal("invalid role credentials were accepted")
		}
	}
}

func TestPrivateConfigAndCredentialParentsMustBe0700(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "config")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "backup.yaml")
	if err := os.WriteFile(path, []byte(minimalBackupYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBackup(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBackup(path); err == nil {
		t.Fatal("public config parent was accepted")
	}
}

func TestFixedRuntimePolicy(t *testing.T) {
	t.Parallel()
	if QuietWindow != 30*time.Second || MaximumQuietWindow != 2*time.Minute || MinimumCheckInterval != 5*time.Minute ||
		FallbackCheckInterval != 12*time.Hour || ConsistencyRetry != 30*time.Second || FailureRetry != 5*time.Minute || DegradedAfter != 30*time.Minute {
		t.Fatal("fixed scheduler policy changed")
	}
}

func writePrivateFile(t *testing.T, contents string) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "credentials")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "private.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
