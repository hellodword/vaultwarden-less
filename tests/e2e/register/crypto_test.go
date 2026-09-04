package register

import (
	"bytes"
	"crypto/rsa"
	"encoding/hex"
	"math/big"
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

func TestEncapsulateKeyUnsignedMatchesRSAOAEPVector(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 64)
	value, err := EncapsulateKeyUnsigned(oaepVectorPublicKey(t), key, repeatReader{value: 7})
	if err != nil {
		t.Fatal(err)
	}
	want := "4.S9h04Bt8Ma8hsvcUTLA7MdmpY0DwQXpRMCP0LumrR/evbV9hBMYu2UmMLD58+aA4bi2s+2DMFur6ee2S7w5DQhKV2acl5196HClM1dHMYhtCLBgerChRXJurswR+saBsb8nw/vCEqu5vQmqLj7y7AmZjvd2Z61ZkfQeztfXmLi1UPhoQTdoHakxc9uaQtlpgmUM0FVUzBcCFRe94ySYL9oKtKB6bANbpTOmTRfROG+g8FvR1avKF+ndgulWDnxg5cmdDQ2MuUW3seqqPj/dEcDLGLSRa/OM+n18BgQBPYP860VeyCcXLYbPSy37MIAIHwAaa2HQgskC/doStb6Co3A=="
	if value != want {
		t.Fatalf("RSA-OAEP vector mismatch\ngot:  %s\nwant: %s", value, want)
	}
}

func oaepVectorPublicKey(t *testing.T) *rsa.PublicKey {
	t.Helper()
	modulus, ok := new(big.Int).SetString("A6EAFA395A25CDDABA1205B1AFA85E9A0AF00B73D04260FC4FD63447140F628045ABE916790369E9E2043532E744E19375C8D6C5A79CA133E94D121BEB687E534CCF2349006685B97E9A62482DDD3F79063715D4B470D06A0CDA7FC6DF84E1A2EA051234AA4364E4758096BA8336AA01F4CD32F9CDBD660764BEAD07BB9D1395E0A0AFFD95FD6D2E250A8A4D0DAAA553037E743D8F4062F514EBD0307EAD91482DAC7EE6CA5D4C1F0D36C5B121E6934E36557D87A2955C7FD9FF603E8A5D3CAAD5A29F3A9124602D5A44AE63F1E7AB5753B98E55127B0F37EEC7C2945A00815DB46BA0DAEEF057340410901415A52846CB104862793558701E22147AA7519CE3", 16)
	if !ok {
		t.Fatal("decode RSA-OAEP vector modulus")
	}
	return &rsa.PublicKey{N: modulus, E: 65537}
}

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
