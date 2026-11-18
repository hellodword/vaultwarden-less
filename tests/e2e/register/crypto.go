// Package register creates account keys for the Vaultwarden registration fixture.
package register

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

const kdfIterations = 600000

func generateSymmetricKey(random io.Reader) ([]byte, error) {
	key := make([]byte, 64)
	if _, err := io.ReadFull(random, key); err != nil {
		return nil, fmt.Errorf("generate symmetric key: %w", err)
	}
	return key, nil
}

// Encrypt returns a Bitwarden type-2 AES-CBC cipher string. The input key is
// the 512-bit symmetric key and is split into 256-bit encryption and MAC keys.
func Encrypt(key, plaintext []byte, random io.Reader) (string, error) {
	if len(key) != 64 {
		return "", fmt.Errorf("symmetric key must be 512 bits")
	}
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return "", err
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(random, iv); err != nil {
		return "", fmt.Errorf("generate cipher IV: %w", err)
	}
	padded := pkcs7Pad(plaintext, block.BlockSize())
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)
	mac := hmac.New(sha256.New, key[32:])
	_, _ = mac.Write(iv)
	_, _ = mac.Write(ciphertext)
	return fmt.Sprintf("2.%s|%s|%s",
		base64.StdEncoding.EncodeToString(iv),
		base64.StdEncoding.EncodeToString(ciphertext),
		base64.StdEncoding.EncodeToString(mac.Sum(nil))), nil
}

type Registration struct {
	Email                  string          `json:"email"`
	KDFType                int             `json:"kdfType"`
	KDFIterations          int             `json:"kdfIterations"`
	Key                    string          `json:"key"`
	MasterPasswordHash     string          `json:"masterPasswordHash"`
	Name                   string          `json:"name"`
	UserAsymmetricKeys     *AsymmetricKeys `json:"userAsymmetricKeys,omitempty"`
	EmailVerificationToken string          `json:"emailVerificationToken,omitempty"`
}

// AsymmetricKeys is the V1 account key pair accepted by Vaultwarden's
// registration endpoint. The private key is an EncString wrapped by the user
// symmetric key; the public key is a base64-encoded DER SubjectPublicKeyInfo.
type AsymmetricKeys struct {
	EncryptedPrivateKey string `json:"encryptedPrivateKey"`
	PublicKey           string `json:"publicKey"`
}

func BuildRegistration(email, password, name string, random io.Reader) (Registration, error) {
	master, err := pbkdf2.Key(sha256.New, password, []byte(strings.ToLower(email)), kdfIterations, 32)
	if err != nil {
		return Registration{}, err
	}
	passwordHash, err := pbkdf2.Key(sha256.New, string(master), []byte(password), 1, 32)
	if err != nil {
		return Registration{}, err
	}
	encryptionKey, err := hkdf.Expand(sha256.New, master, "enc", 32)
	if err != nil {
		return Registration{}, err
	}
	macKey, err := hkdf.Expand(sha256.New, master, "mac", 32)
	if err != nil {
		return Registration{}, err
	}
	symmetric, err := generateSymmetricKey(random)
	if err != nil {
		return Registration{}, err
	}
	wrapped, err := Encrypt(append(encryptionKey, macKey...), symmetric, random)
	if err != nil {
		return Registration{}, err
	}
	asymmetricKeys, err := buildAsymmetricKeys(symmetric, random)
	if err != nil {
		return Registration{}, err
	}
	return Registration{
		Email: email, KDFType: 0, KDFIterations: kdfIterations,
		Key: wrapped, MasterPasswordHash: base64.StdEncoding.EncodeToString(passwordHash), Name: name,
		UserAsymmetricKeys: asymmetricKeys,
	}, nil
}

func buildAsymmetricKeys(wrappingKey []byte, random io.Reader) (*AsymmetricKeys, error) {
	privateKey, err := rsa.GenerateKey(random, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate asymmetric key: %w", err)
	}
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("marshal asymmetric private key: %w", err)
	}
	publicKeyDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal asymmetric public key: %w", err)
	}
	encryptedPrivateKey, err := Encrypt(wrappingKey, privateKeyDER, random)
	if err != nil {
		return nil, fmt.Errorf("encrypt asymmetric private key: %w", err)
	}
	return &AsymmetricKeys{
		EncryptedPrivateKey: encryptedPrivateKey,
		PublicKey:           base64.StdEncoding.EncodeToString(publicKeyDER),
	}, nil
}

func pkcs7Pad(data []byte, size int) []byte {
	padding := size - len(data)%size
	result := make([]byte, len(data)+padding)
	copy(result, data)
	for i := len(data); i < len(result); i++ {
		result[i] = byte(padding)
	}
	return result
}
