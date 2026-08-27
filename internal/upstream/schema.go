package upstream

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
)

type SchemaObject struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Table string `json:"table"`
	SQL   string `json:"sql"`
}

type SchemaContract struct {
	ContractVersion    int            `json:"contract_version"`
	VaultwardenVersion string         `json:"vaultwarden_version"`
	DatabaseMigration  string         `json:"database_migration"`
	Fingerprint        string         `json:"fingerprint"`
	Objects            []SchemaObject `json:"objects"`
}

//go:embed vaultwarden-schema.json
var schemaContractJSON []byte

var (
	loadSchemaOnce  sync.Once
	loadedSchema    SchemaContract
	loadedSchemaErr error
)

func VaultwardenSchemaContract() (SchemaContract, error) {
	loadSchemaOnce.Do(func() {
		if err := decodeStrictJSON(schemaContractJSON, &loadedSchema); err != nil {
			loadedSchemaErr = fmt.Errorf("decode embedded Vaultwarden schema contract: %w", err)
			return
		}
		if loadedSchema.ContractVersion != 1 ||
			loadedSchema.VaultwardenVersion == "" ||
			loadedSchema.DatabaseMigration == "" ||
			!validDigest(loadedSchema.Fingerprint) ||
			len(loadedSchema.Objects) == 0 ||
			SchemaFingerprint(loadedSchema.Objects) != loadedSchema.Fingerprint {
			loadedSchemaErr = fmt.Errorf("embedded Vaultwarden schema contract is inconsistent")
			return
		}
		for index, object := range loadedSchema.Objects {
			if object.Type == "" || object.Name == "" || object.Table == "" || object.SQL == "" {
				loadedSchemaErr = fmt.Errorf("embedded Vaultwarden schema object %d is incomplete", index)
				return
			}
			if index > 0 {
				previous := loadedSchema.Objects[index-1]
				if previous.Type > object.Type || previous.Type == object.Type && previous.Name >= object.Name {
					loadedSchemaErr = fmt.Errorf("embedded Vaultwarden schema objects are not strictly sorted")
					return
				}
			}
		}
	})
	result := loadedSchema
	result.Objects = slices.Clone(loadedSchema.Objects)
	return result, loadedSchemaErr
}

func SchemaFingerprint(objects []SchemaObject) string {
	digest := sha256.New()
	for _, object := range objects {
		for _, value := range []string{object.Type, object.Name, object.Table, object.SQL} {
			_, _ = digest.Write([]byte(value))
			_, _ = digest.Write([]byte{0})
		}
		_, _ = digest.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("document contains trailing JSON")
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	raw := strings.TrimPrefix(value, "sha256:")
	if strings.ToLower(raw) != raw {
		return false
	}
	_, err := hex.DecodeString(raw)
	return err == nil
}
