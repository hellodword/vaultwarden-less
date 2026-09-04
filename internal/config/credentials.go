package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hellodword/vaultwarden-less/internal/privatefs"
)

var (
	environmentPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
	forbiddenEnvNames  = map[string]struct{}{
		"PATH": {}, "HOME": {}, "TMPDIR": {}, "TMP": {}, "TEMP": {},
		"LD_PRELOAD": {}, "LD_LIBRARY_PATH": {}, "GODEBUG": {},
	}
)

type RoleTarget struct {
	Environment map[string]string `yaml:"environment"`
}

type RoleCredentials struct {
	Targets map[string]RoleTarget `yaml:"targets"`
}

func LoadRoleCredentials(path string, cfg *Backup) (*RoleCredentials, error) {
	if cfg == nil {
		return nil, fmt.Errorf("backup config is required")
	}
	data, err := privatefs.ReadBytes(path, MaxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("read role credentials: %w", err)
	}
	var credentials RoleCredentials
	if err := decodeOneYAML(data, &credentials); err != nil {
		return nil, fmt.Errorf("decode role credentials: invalid YAML")
	}
	if err := credentials.Validate(cfg); err != nil {
		return nil, fmt.Errorf("validate role credentials: %w", err)
	}
	return &credentials, nil
}

func (c RoleCredentials) Validate(cfg *Backup) error {
	if cfg == nil || c.Targets == nil || len(c.Targets) != len(cfg.Targets) {
		return fmt.Errorf("target key set must exactly match backup config")
	}
	for _, target := range cfg.Targets {
		value, exists := c.Targets[target.Name]
		if !exists {
			return fmt.Errorf("target key set must exactly match backup config")
		}
		if err := value.validate(); err != nil {
			return fmt.Errorf("target %s: %w", target.Name, err)
		}
	}
	return nil
}

func (c RoleCredentials) Environment(target Target) (map[string]string, error) {
	values, exists := c.Targets[target.Name]
	if !exists {
		return nil, fmt.Errorf("target entry is missing")
	}
	if err := values.validate(); err != nil {
		return nil, err
	}
	result := make(map[string]string, len(values.Environment))
	for name, value := range values.Environment {
		result[name] = value
	}
	return result, nil
}

func (target RoleTarget) validate() error {
	if target.Environment == nil || len(target.Environment) > 128 {
		return fmt.Errorf("environment must contain at most 128 entries")
	}
	for name, value := range target.Environment {
		if !environmentPattern.MatchString(name) {
			return fmt.Errorf("environment name is invalid")
		}
		if strings.HasPrefix(name, "RESTIC_") {
			return fmt.Errorf("environment name is reserved")
		}
		if _, forbidden := forbiddenEnvNames[name]; forbidden {
			return fmt.Errorf("environment name is reserved")
		}
		if value == "" || len(value) > 64<<10 || strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("environment value is invalid")
		}
	}
	return nil
}

func ReadPassword(root, target string) (string, error) {
	if !namePattern.MatchString(target) {
		return "", fmt.Errorf("invalid target name")
	}
	if err := privatefs.ValidatePrivateDirectory(root); err != nil {
		return "", fmt.Errorf("validate password directory: %w", err)
	}
	path := filepath.Join(root, target)
	if filepath.Dir(path) != filepath.Clean(root) {
		return "", fmt.Errorf("invalid password path")
	}
	data, err := privatefs.ReadBytes(path, MaxPasswordBytes)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	if len(data) == 0 || bytes.IndexByte(data, 0) >= 0 || bytes.IndexByte(data, '\r') >= 0 {
		return "", fmt.Errorf("password must be one nonempty line")
	}
	if bytes.HasSuffix(data, []byte{'\n'}) {
		data = data[:len(data)-1]
	}
	if len(data) == 0 || bytes.IndexByte(data, '\n') >= 0 {
		return "", fmt.Errorf("password must be one nonempty line")
	}
	return string(data), nil
}

func ValidatePasswordDirectory(root string, cfg *Backup) error {
	if cfg == nil {
		return fmt.Errorf("backup config is required")
	}
	if err := privatefs.ValidatePrivateDirectory(root); err != nil {
		return fmt.Errorf("validate password directory: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("read password directory: %w", err)
	}
	expected := make(map[string]struct{}, len(cfg.Targets))
	for _, target := range cfg.Targets {
		expected[target.Name] = struct{}{}
	}
	actual := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if _, exists := expected[entry.Name()]; !exists || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return fmt.Errorf("password file set must exactly match backup config")
		}
		actual[entry.Name()] = struct{}{}
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("password file set must exactly match backup config")
	}
	for _, target := range cfg.Targets {
		if _, err := ReadPassword(root, target.Name); err != nil {
			return fmt.Errorf("target %s: %w", target.Name, err)
		}
	}
	return nil
}
