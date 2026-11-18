package register

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestEncryptMatchesAESCBCHMACVector(t *testing.T) {
	encryptionKey, err := hex.DecodeString("603deb1015ca71be2b73aef0857d77811f352c073b6108d72d9810a30914dff4")
	if err != nil {
		t.Fatal(err)
	}
	macKey, err := hex.DecodeString("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := hex.DecodeString("6bc1bee22e409f96e93d7e117393172a")
	if err != nil {
		t.Fatal(err)
	}
	iv, err := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Encrypt(append(encryptionKey, macKey...), plaintext, bytes.NewReader(iv))
	if err != nil {
		t.Fatal(err)
	}
	want := "2.AAECAwQFBgcICQoLDA0ODw==|9YxMBNbl8bp3nqv7X3v71khaXIFRnPN4+jbUK4VH7cA=|/ZXuY8JPehCdvifkTZHxBeeQ5+XNhr1KEdlbe/HK1tE="
	if got != want {
		t.Fatalf("AES-CBC-HMAC vector mismatch\ngot:  %s\nwant: %s", got, want)
	}
}
