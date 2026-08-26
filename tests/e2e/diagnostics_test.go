package e2e_test

import (
	"strings"
	"testing"
)

func redactDiagnostics(value string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "<redacted>")
		}
	}
	return truncateDiagnostic(value, 6000)
}

func containsSecret(value string, secrets ...string) bool {
	for _, secret := range secrets {
		if secret != "" && strings.Contains(value, secret) {
			return true
		}
	}
	return false
}

func truncateDiagnostic(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func TestRedactDiagnosticsRemovesEverySecretAndBoundsOutput(t *testing.T) {
	t.Parallel()
	value := strings.Repeat("prefix-", 1000) + "password/session"
	redacted := redactDiagnostics(value, "password", "session")
	if strings.Contains(redacted, "password") || strings.Contains(redacted, "session") {
		t.Fatal("diagnostic redaction retained a secret")
	}
	if len(redacted) > 6003 || !strings.HasSuffix(redacted, "...") {
		t.Fatalf("diagnostic bound was not enforced: %d", len(redacted))
	}
}
