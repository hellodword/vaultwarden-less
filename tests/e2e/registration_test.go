package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/tests/e2e/register"
)

// registerAccount is a narrow protocol fixture because the official
// Bitwarden CLI deliberately does not expose account registration.
// Every ordinary vault operation uses the Nix-provided official bw.
func registerAccount(ctx context.Context, t *testing.T, server, email, password string) {
	t.Helper()
	verification, err := json.Marshal(map[string]string{"email": email, "name": "Vaultwarden-less E2E"})
	if err != nil {
		t.Fatal(err)
	}
	status, body := registrationPost(ctx, t, server, "/identity/accounts/register/send-verification-email", verification)
	if status != http.StatusOK {
		t.Fatalf("registration verification endpoint returned %d", status)
	}
	var token string
	if err := json.Unmarshal(body, &token); err != nil || token == "" {
		t.Fatalf("registration verification token was invalid")
	}
	registration, err := register.BuildRegistration(email, password, "Vaultwarden-less E2E", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	registration.EmailVerificationToken = token
	data, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}
	status, _ = registrationPost(ctx, t, server, "/identity/accounts/register/finish", data)
	if status != http.StatusOK {
		t.Fatalf("registration finish returned %d", status)
	}
}

func expectRegistrationRejected(ctx context.Context, t *testing.T, server, email, password string) {
	t.Helper()
	verification, err := json.Marshal(map[string]string{"email": email, "name": "Vaultwarden-less E2E"})
	if err != nil {
		t.Fatal(err)
	}
	status, body := registrationPost(ctx, t, server, "/identity/accounts/register/send-verification-email", verification)
	if status >= 400 && status < 500 {
		return
	}
	if status != http.StatusOK {
		t.Fatalf("disabled registration verification endpoint returned %d", status)
	}
	var token string
	if err := json.Unmarshal(body, &token); err != nil || token == "" {
		t.Fatalf("disabled registration verification token was invalid")
	}
	registration, err := register.BuildRegistration(email, password, "Vaultwarden-less E2E", rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	registration.EmailVerificationToken = token
	data, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}
	status, _ = registrationPost(ctx, t, server, "/identity/accounts/register/finish", data)
	if status < 400 || status >= 500 {
		t.Fatalf("disabled registration finish returned %d", status)
	}
}

func registrationPost(ctx context.Context, t *testing.T, server, path string, body []byte) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := (&http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, readError := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readError != nil {
		t.Fatal(readError)
	}
	return response.StatusCode, data
}
