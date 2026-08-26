//go:build systemtest

package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hellodword/vaultwarden-less/tests/e2e/register"
)

// registerAccount is the only protocol fixture in the E2E because the
// official Bitwarden CLI deliberately does not expose account registration.
// Every vault operation after this point uses the Nix-provided official bw.
func registerAccount(ctx context.Context, t *testing.T, server, email, password string) register.Registration {
	t.Helper()
	post := func(path string, body []byte) (int, []byte) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json")
		response, err := (&http.Client{Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
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
	verification, err := json.Marshal(map[string]string{"email": email, "name": "Vaultwarden-less E2E"})
	if err != nil {
		t.Fatal(err)
	}
	status, body := post("/identity/accounts/register/send-verification-email", verification)
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
	data, err := registration.WithEmailVerificationToken(token).JSON()
	if err != nil {
		t.Fatal(err)
	}
	status, _ = post("/identity/accounts/register/finish", data)
	if status != http.StatusOK {
		t.Fatalf("registration finish returned %d", status)
	}
	return registration
}

// createOrganization is the second and final narrow protocol fixture. The
// official CLI can manage organizations and collections but cannot create the
// initial organization. All subsequent org operations use the pinned CLI.
func createOrganization(
	ctx context.Context,
	t *testing.T,
	server, email, name, collectionName string,
	registration register.Registration,
) string {
	t.Helper()
	deviceID := randomUUID(t)
	form := url.Values{
		"grant_type":        {"password"},
		"client_id":         {"cli"},
		"scope":             {"api offline_access"},
		"username":          {email},
		"password":          {registration.MasterPasswordHash},
		"device_identifier": {deviceID},
		"device_name":       {"Vaultwarden-less E2E fixture"},
		"device_type":       {"25"},
	}
	tokenRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		server+"/identity/connect/token",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		t.Fatal(err)
	}
	tokenRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenRequest.Header.Set("Device-Type", "25")
	status, tokenBody := executeFixtureRequest(t, tokenRequest)
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if status != http.StatusOK || json.Unmarshal(tokenBody, &token) != nil || token.AccessToken == "" {
		t.Fatalf("fixture login returned %d or an invalid token", status)
	}

	organizationKey, err := register.GenerateSymmetricKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrappedKey, err := register.EncapsulateKeyUnsigned(registration.UserPublicKey, organizationKey, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	organizationKeys, _, err := register.BuildAsymmetricKeys(organizationKey, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encryptedCollection, err := register.Encrypt(organizationKey, []byte(collectionName), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"billingEmail":   email,
		"collectionName": encryptedCollection,
		"key":            wrappedKey,
		"keys":           organizationKeys,
		"name":           name,
		"planType":       0,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		server+"/api/organizations",
		bytes.NewReader(payload),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Device-Type", "25")
	status, body := executeFixtureRequest(t, request)
	var organization map[string]any
	if status != http.StatusOK || json.Unmarshal(body, &organization) != nil {
		t.Fatalf("organization creation returned %d or invalid JSON", status)
	}
	id, _ := organization["id"].(string)
	if id == "" {
		t.Fatal("organization creation returned no ID")
	}
	return id
}

func executeFixtureRequest(t *testing.T, request *http.Request) (int, []byte) {
	t.Helper()
	response, err := (&http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func randomUUID(t *testing.T) string {
	t.Helper()
	var value [16]byte
	if _, err := io.ReadFull(rand.Reader, value[:]); err != nil {
		t.Fatal(err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16],
	)
}
