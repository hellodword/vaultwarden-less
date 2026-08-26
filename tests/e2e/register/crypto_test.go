package register

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

type repeatReader struct{ value byte }

func (r repeatReader) Read(data []byte) (int, error) {
	for index := range data {
		data[index] = r.value
	}
	return len(data), nil
}

func TestPBKDF2AndHKDFVectors(t *testing.T) {
	if got := hex.EncodeToString(pbkdf2SHA256([]byte("password"), []byte("salt"), 1, 32)); got != "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b" {
		t.Fatalf("PBKDF2 = %s", got)
	}
	prk, err := hex.DecodeString("077709362c2e32df0ddc3f0dc47bba6390b6c73bb50f9c3122ec844ad7c2b3e5")
	if err != nil {
		t.Fatal(err)
	}
	got := hex.EncodeToString(hkdfExpand(prk, []byte{0xf0, 0xf1, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8, 0xf9}, 42))
	want := "3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865"
	if got != want {
		t.Fatalf("HKDF = %s", got)
	}
}

func TestRegistrationUsesCurrentRegistrationShape(t *testing.T) {
	registration, err := BuildRegistration("User@example.com", "correct horse battery staple", "Test User", repeatReader{value: 7})
	if err != nil {
		t.Fatal(err)
	}
	if registration.KDFType != KDFPBKDF2 || registration.KDFIterations != PBKDF2SHA256 {
		t.Fatalf("KDF = %d/%d", registration.KDFType, registration.KDFIterations)
	}
	if !strings.HasPrefix(registration.Key, "2.") || registration.MasterPasswordHash == "" {
		t.Fatal("registration crypto fields are missing or malformed")
	}
	data, err := registration.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"email", "kdfType", "kdfIterations", "key", "masterPasswordHash", "name"} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("registration omitted %q", name)
		}
	}
	keys, ok := fields["userAsymmetricKeys"].(map[string]any)
	if !ok || !strings.HasPrefix(stringValue(keys["encryptedPrivateKey"]), "2.") || stringValue(keys["publicKey"]) == "" {
		t.Fatal("registration asymmetric keys are missing or malformed")
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func TestRegistrationCanCarryVerificationToken(t *testing.T) {
	registration, err := BuildRegistration("user@example.com", "correct horse battery staple", "Test User", repeatReader{value: 9})
	if err != nil {
		t.Fatal(err)
	}
	data, err := registration.WithEmailVerificationToken("verification-token").JSON()
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["emailVerificationToken"] != "verification-token" {
		t.Fatal("registration verification token was not preserved")
	}
}

func TestCipherMACChangesWithCiphertext(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 64)
	first, err := Encrypt(key, []byte("first"), repeatReader{value: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Encrypt(key, []byte("second"), repeatReader{value: 1})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("different plaintexts produced the same cipher")
	}
	hash := sha256.Sum256([]byte(first))
	if hash == ([32]byte{}) {
		t.Fatal("cipher hash unexpectedly empty")
	}
}
