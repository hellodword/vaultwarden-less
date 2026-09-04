// Package register contains the small, standard-library-only V1 crypto builder
// used by the registration E2E fixture. Its protocol
// fields are reviewed against the Bitwarden CLI and Vaultwarden commits in
// UPSTREAMS.toml. It intentionally does not implement a general client.
package register

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	PBKDF2SHA256 = 600000
	KDFPBKDF2    = 0
	CipherType2  = 2
	CipherType4  = 4
)

type Keys struct {
	MasterKey           []byte
	MasterPasswordHash  []byte
	StretchedEncryption []byte
	StretchedMAC        []byte
}

func DeriveKeys(email, password string) Keys {
	master := pbkdf2SHA256([]byte(password), []byte(strings.ToLower(email)), PBKDF2SHA256, 32)
	passwordHash := pbkdf2SHA256(master, []byte(password), 1, 32)
	return Keys{
		MasterKey:           master,
		MasterPasswordHash:  passwordHash,
		StretchedEncryption: hkdfExpand(master, []byte("enc"), 32),
		StretchedMAC:        hkdfExpand(master, []byte("mac"), 32),
	}
}

func GenerateSymmetricKey(random io.Reader) ([]byte, error) {
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
	return fmt.Sprintf("%d.%s|%s|%s", CipherType2,
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
	SymmetricKey           []byte          `json:"-"`
	UserPublicKey          *rsa.PublicKey  `json:"-"`
}

// AsymmetricKeys is the V1 account key pair accepted by Vaultwarden's
// registration endpoint. The private key is an EncString wrapped by the user
// symmetric key; the public key is a base64-encoded DER SubjectPublicKeyInfo.
type AsymmetricKeys struct {
	EncryptedPrivateKey string `json:"encryptedPrivateKey"`
	PublicKey           string `json:"publicKey"`
}

func (r Registration) WithEmailVerificationToken(token string) Registration {
	r.EmailVerificationToken = token
	return r
}

func BuildRegistration(email, password, name string, random io.Reader) (Registration, error) {
	keys := DeriveKeys(email, password)
	symmetric, err := GenerateSymmetricKey(random)
	if err != nil {
		return Registration{}, err
	}
	wrapped, err := Encrypt(append(append([]byte(nil), keys.StretchedEncryption...), keys.StretchedMAC...), symmetric, random)
	if err != nil {
		return Registration{}, err
	}
	asymmetricKeys, publicKey, err := BuildAsymmetricKeys(symmetric, random)
	if err != nil {
		return Registration{}, err
	}
	return Registration{
		Email: email, KDFType: KDFPBKDF2, KDFIterations: PBKDF2SHA256,
		Key: wrapped, MasterPasswordHash: base64.StdEncoding.EncodeToString(keys.MasterPasswordHash), Name: name,
		SymmetricKey: symmetric, UserPublicKey: publicKey,
		UserAsymmetricKeys: asymmetricKeys,
	}, nil
}

func BuildAsymmetricKeys(wrappingKey []byte, random io.Reader) (*AsymmetricKeys, *rsa.PublicKey, error) {
	privateKey, err := rsa.GenerateKey(random, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate asymmetric key: %w", err)
	}
	privateKeyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal asymmetric private key: %w", err)
	}
	publicKeyDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal asymmetric public key: %w", err)
	}
	encryptedPrivateKey, err := Encrypt(wrappingKey, privateKeyDER, random)
	if err != nil {
		return nil, nil, fmt.Errorf("encrypt asymmetric private key: %w", err)
	}
	return &AsymmetricKeys{
		EncryptedPrivateKey: encryptedPrivateKey,
		PublicKey:           base64.StdEncoding.EncodeToString(publicKeyDER),
	}, &privateKey.PublicKey, nil
}

// EncapsulateKeyUnsigned implements the Bitwarden V1 RSA-OAEP-SHA1 key
// encapsulation contract. SHA-1 is fixed by EncString type 4; it is not used
// for hashing or signatures here.
func EncapsulateKeyUnsigned(publicKey *rsa.PublicKey, key []byte, random io.Reader) (string, error) {
	if publicKey == nil {
		return "", fmt.Errorf("public key is required")
	}
	if len(key) != 64 {
		return "", fmt.Errorf("shared key must be 512 bits")
	}
	ciphertext, err := rsa.EncryptOAEP(sha1.New(), random, publicKey, key, nil)
	if err != nil {
		return "", fmt.Errorf("encapsulate shared key: %w", err)
	}
	return fmt.Sprintf("%d.%s", CipherType4, base64.StdEncoding.EncodeToString(ciphertext)), nil
}

func (r Registration) JSON() ([]byte, error) { return json.Marshal(r) }

func pbkdf2SHA256(password, salt []byte, iterations, length int) []byte {
	result := make([]byte, 0, length)
	for block := uint32(1); len(result) < length; block++ {
		h := hmac.New(sha256.New, password)
		_, _ = h.Write(salt)
		_, _ = h.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
		u := h.Sum(nil)
		t := append([]byte(nil), u...)
		for count := 1; count < iterations; count++ {
			h = hmac.New(sha256.New, password)
			_, _ = h.Write(u)
			u = h.Sum(nil)
			for i := range t {
				t[i] ^= u[i]
			}
		}
		result = append(result, t...)
	}
	return result[:length]
}

func hkdfExpand(prk, info []byte, length int) []byte {
	result := make([]byte, 0, length)
	previous := []byte{}
	for counter := byte(1); len(result) < length; counter++ {
		h := hmac.New(sha256.New, prk)
		_, _ = h.Write(previous)
		_, _ = h.Write(info)
		_, _ = h.Write([]byte{counter})
		previous = h.Sum(nil)
		result = append(result, previous...)
	}
	return result[:length]
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
