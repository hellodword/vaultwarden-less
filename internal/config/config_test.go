package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTrackedConfigsValidate(t *testing.T) {
	t.Parallel()
	if _, err := LoadProxy(filepath.Join("..", "..", "config", "proxy.json")); err != nil {
		t.Fatalf("proxy config: %v", err)
	}
	if _, err := LoadBackup(filepath.Join("..", "..", "config", "backup.json")); err != nil {
		t.Fatalf("backup config: %v", err)
	}
}

func TestProxyRejectsUnknownFieldsAndWrongFixedLimit(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "proxy.json"))
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Replace(string(data), "\n}", ",\n  \"unexpected\": true\n}", 1)
	if _, err := DecodeProxy([]byte(unknown)); err == nil {
		t.Fatal("unknown field was accepted")
	}
	wrongLimit := strings.Replace(string(data), "629145600", "629145599", 1)
	if _, err := DecodeProxy([]byte(wrongLimit)); err == nil {
		t.Fatal("wrong request limit was accepted")
	}
	queryUpstream := strings.Replace(string(data), `"upstream_url": "http://vaultwarden:80"`, `"upstream_url": "http://vaultwarden:80?unsafe=true"`, 1)
	if _, err := DecodeProxy([]byte(queryUpstream)); err == nil {
		t.Fatal("upstream query was accepted")
	}
	nonNetworkCIDR := strings.Replace(string(data), `"172.16.0.0/12"`, `"172.16.1.0/12"`, 1)
	if _, err := DecodeProxy([]byte(nonNetworkCIDR)); err == nil {
		t.Fatal("CIDR with host bits was accepted")
	}
}

func TestBackupRejectsUnsupportedBackendAndOverlappingPaths(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "backup.json"))
	if err != nil {
		t.Fatal(err)
	}
	unsupported := strings.Replace(string(data), `"backend": "rest"`, `"backend": "sftp"`, 1)
	if _, err := DecodeBackup([]byte(unsupported)); err == nil {
		t.Fatal("SFTP backend was accepted")
	}
	overlap := strings.Replace(string(data), `"state_dir": "/state"`, `"state_dir": "/data/state"`, 1)
	if _, err := DecodeBackup([]byte(overlap)); err == nil {
		t.Fatal("overlapping state path was accepted")
	}
}

func TestDurationRejectsNonPositive(t *testing.T) {
	t.Parallel()
	for _, value := range []string{`"0s"`, `"-1s"`, `1`} {
		var duration Duration
		if err := duration.UnmarshalJSON([]byte(value)); err == nil {
			t.Fatalf("accepted invalid duration %s", value)
		}
	}
}

func TestTargetOptionsAreBackendSpecificAndBounded(t *testing.T) {
	t.Parallel()
	target := Target{
		Name: "cos", Backend: "s3", RepositoryFile: "/repository",
		Options:           map[string]string{"s3.bucket-lookup": "dns", "s3.region": "ap-singapore"},
		BackupCredentials: Credentials{PasswordFile: "/password"},
	}
	if err := target.validate(); err != nil {
		t.Fatal(err)
	}
	target.Options["s3.bucket-lookup"] = "shell command"
	if err := target.validate(); err == nil {
		t.Fatal("unsafe S3 option was accepted")
	}
	target.Backend = "rest"
	target.Options = map[string]string{"s3.bucket-lookup": "dns"}
	if err := target.validate(); err == nil {
		t.Fatal("S3 option was accepted for REST backend")
	}
}

func TestBarkAcceptsOnlyOfficialAPNsOrigins(t *testing.T) {
	t.Parallel()
	notifier := Notifier{
		Name: "bark", Type: "bark", Timeout: Duration{Duration: 10 * time.Second},
		Endpoint: "https://api.push.apple.com", DeviceTokenFile: "/device-token", AuthKeyFile: "/auth-key",
		KeyID: "KEY_ID", TeamID: "TEAM_ID", Topic: "me.fin.bark",
	}
	if err := notifier.validate(); err != nil {
		t.Fatal(err)
	}
	notifier.Endpoint = "https://apns.example.test"
	if err := notifier.validate(); err == nil {
		t.Fatal("non-Apple APNs endpoint was accepted")
	}
}
