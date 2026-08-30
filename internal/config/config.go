package config

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/hellodword/vaultwarden-less/internal/privatefs"
)

const (
	MaxConfigBytes   = 1 << 20
	MaxPasswordBytes = 64 << 10

	DefaultBackupConfigPath = "/config/backup.yaml"
	VaultwardenEnvPath      = "/deployment/vaultwarden.env"
	RoleCredentialsPath     = "/run/credentials/role.yaml"
	ResticPasswordRoot      = "/run/credentials/restic"
	NotifierCredentialsPath = "/run/credentials/notifiers.yaml"
	DeploymentRoot          = "/deployment-root"
	ImportRoot              = "/import"

	ListenAddress     = "0.0.0.0:9090"
	WorkerURL         = "http://backup:9090"
	DataDir           = "/data"
	StagingDir        = "/staging"
	RecoveryPointPath = "/staging/point"
	CacheDir          = "/cache"
	RestoreRoot       = "/restore"
	StateDir          = "/state"
	ResticBinary      = "/bin/restic"

	QuietWindow           = 30 * time.Second
	MaximumQuietWindow    = 2 * time.Minute
	MinimumCheckInterval  = 5 * time.Minute
	FallbackCheckInterval = 12 * time.Hour
	ConsistencyRetry      = 30 * time.Second
	FailureRetry          = 5 * time.Minute
	DegradedAfter         = 30 * time.Minute
	EventRetry            = 30 * time.Second
	TargetTimeout         = 2 * time.Hour
	MaintenanceTimeout    = 24 * time.Hour
	NotifierTimeout       = 10 * time.Second
	ShutdownTimeout       = 2 * time.Minute
	EventMaxBytes         = 1024
)

var (
	namePattern        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	environmentPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
	canonicalDecimal   = regexp.MustCompile(`^[1-9][0-9]*$`)
	forbiddenEnvNames  = map[string]struct{}{
		"PATH": {}, "HOME": {}, "TMPDIR": {}, "TMP": {}, "TEMP": {},
		"LD_PRELOAD": {}, "LD_LIBRARY_PATH": {}, "GODEBUG": {},
	}
)

var errUnknownYAMLField = errors.New("unknown YAML field")

// Backup is the complete human-authored configuration. Scheduling,
// retry, timeout, retention, filesystem and HTTP behavior are product constants.
type Backup struct {
	UpgradeVerificationTargets []string   `yaml:"upgrade_verification_targets"`
	Targets                    []Target   `yaml:"targets"`
	Notifiers                  []Notifier `yaml:"notifiers"`
}

type Target struct {
	Name       string   `yaml:"name"`
	Repository string   `yaml:"repository"`
	Options    []Option `yaml:"options"`
}

type Option struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type Notifier struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`

	WebhookURL string `yaml:"-"`

	DeviceToken string `yaml:"-"`
	AuthKey     string `yaml:"-"`
	KeyID       string `yaml:"-"`
	TeamID      string `yaml:"-"`
	Topic       string `yaml:"-"`
	Sound       string `yaml:"-"`
}

type NotifierSecrets struct {
	WebhookURL *string `yaml:"webhook_url"`

	DeviceToken *string `yaml:"device_token"`
	AuthKey     *string `yaml:"auth_key"`
	KeyID       *string `yaml:"key_id"`
	TeamID      *string `yaml:"team_id"`
	Topic       *string `yaml:"topic"`
	Sound       *string `yaml:"sound"`
}

type NotifierCredentials struct {
	Notifiers map[string]NotifierSecrets `yaml:"notifiers"`
}

type RoleTarget struct {
	Environment map[string]string `yaml:"environment"`
}

type RoleCredentials struct {
	Targets map[string]RoleTarget `yaml:"targets"`
}

type Deployment struct {
	Root       string
	RuntimeUID uint32
	RuntimeGID uint32
	ListenPort uint16
	DrillPort  uint16
}

func LoadBackup(path string) (*Backup, error) {
	data, err := privatefs.ReadBytes(path, MaxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("read backup config: %w", err)
	}
	return DecodeBackup(data)
}

func DecodeBackup(data []byte) (*Backup, error) {
	if len(data) > MaxConfigBytes {
		return nil, fmt.Errorf("backup config exceeds %d bytes", MaxConfigBytes)
	}
	var cfg Backup
	if err := decodeOneYAML(data, &cfg); err != nil {
		if errors.Is(err, errUnknownYAMLField) {
			return nil, fmt.Errorf("decode backup config: unknown field")
		}
		return nil, fmt.Errorf("decode backup config: invalid YAML")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate backup config: %w", err)
	}
	return &cfg, nil
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

func LoadNotifierCredentials(path string, cfg *Backup) ([]Notifier, error) {
	if cfg == nil {
		return nil, fmt.Errorf("backup config is required")
	}
	data, err := privatefs.ReadBytes(path, MaxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("read notifier credentials: %w", err)
	}
	var credentials NotifierCredentials
	if err := decodeOneYAML(data, &credentials); err != nil {
		return nil, fmt.Errorf("decode notifier credentials: invalid YAML")
	}
	resolved, err := credentials.Resolve(cfg)
	if err != nil {
		return nil, fmt.Errorf("validate notifier credentials: %w", err)
	}
	return resolved, nil
}

func decodeOneYAML(data []byte, destination any) error {
	if len(data) == 0 || !utf8.Valid(data) {
		return fmt.Errorf("invalid UTF-8 or empty document")
	}
	file, err := parser.ParseBytes(data, 0)
	if err != nil || len(file.Docs) != 1 || file.Docs[0] == nil || file.Docs[0].Body == nil {
		return fmt.Errorf("invalid YAML document")
	}
	visitor := &strictYAMLVisitor{}
	ast.Walk(visitor, file.Docs[0])
	if visitor.err != nil {
		return fmt.Errorf("YAML must use explicit strings")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data), yaml.DisallowUnknownField())
	if err := decoder.Decode(destination); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unknown field") {
			return errUnknownYAMLField
		}
		return err
	}
	return nil
}

type strictYAMLVisitor struct {
	err error
}

func (visitor *strictYAMLVisitor) Visit(node ast.Node) ast.Visitor {
	if node == nil || visitor.err != nil {
		return nil
	}
	switch node.Type() {
	case ast.AnchorType, ast.AliasType, ast.MergeKeyType, ast.TagType, ast.DirectiveType,
		ast.NullType, ast.BoolType, ast.FloatType, ast.InfinityType, ast.NanType:
		visitor.err = fmt.Errorf("forbidden YAML node")
		return nil
	case ast.IntegerType:
		visitor.err = fmt.Errorf("implicit scalar conversion is forbidden")
		return nil
	}
	return visitor
}

func (c *Backup) Validate() error {
	if len(c.Targets) < 1 || len(c.Targets) > 32 {
		return fmt.Errorf("targets must contain 1 to 32 entries")
	}
	targetNames := make(map[string]struct{}, len(c.Targets))
	for index := range c.Targets {
		if err := c.Targets[index].Validate(); err != nil {
			return fmt.Errorf("targets[%d]: %w", index, err)
		}
		if _, duplicate := targetNames[c.Targets[index].Name]; duplicate {
			return fmt.Errorf("target names must be unique")
		}
		targetNames[c.Targets[index].Name] = struct{}{}
	}
	if len(c.UpgradeVerificationTargets) < 1 || len(c.UpgradeVerificationTargets) > 32 {
		return fmt.Errorf("upgrade_verification_targets must contain 1 to 32 entries")
	}
	verificationNames := make(map[string]struct{}, len(c.UpgradeVerificationTargets))
	for _, name := range c.UpgradeVerificationTargets {
		if _, exists := targetNames[name]; !exists {
			return fmt.Errorf("upgrade verification target %q is not configured", name)
		}
		if _, duplicate := verificationNames[name]; duplicate {
			return fmt.Errorf("upgrade verification target names must be unique")
		}
		verificationNames[name] = struct{}{}
	}
	if c.Notifiers == nil || len(c.Notifiers) > 16 {
		return fmt.Errorf("notifiers must be an array containing 0 to 16 entries")
	}
	notifierNames := make(map[string]struct{}, len(c.Notifiers))
	for index := range c.Notifiers {
		if err := c.Notifiers[index].validateReference(); err != nil {
			return fmt.Errorf("notifiers[%d]: %w", index, err)
		}
		if _, duplicate := notifierNames[c.Notifiers[index].Name]; duplicate {
			return fmt.Errorf("notifier names must be unique")
		}
		notifierNames[c.Notifiers[index].Name] = struct{}{}
	}
	return nil
}

func (c Backup) Target(name string) (Target, bool) {
	for _, target := range c.Targets {
		if target.Name == name {
			return target, true
		}
	}
	return Target{}, false
}

func (t Target) Validate() error {
	if !namePattern.MatchString(t.Name) {
		return fmt.Errorf("invalid name %q", t.Name)
	}
	if len(t.Repository) < 1 || len(t.Repository) > 4096 || strings.ContainsAny(t.Repository, "\r\n\x00") {
		return fmt.Errorf("repository must be a nonempty string of at most 4096 bytes")
	}
	if t.Options == nil || len(t.Options) > 64 {
		return fmt.Errorf("options must be an array containing at most 64 entries")
	}
	optionNames := make(map[string]struct{}, len(t.Options))
	for index, option := range t.Options {
		if len(option.Name) < 1 || len(option.Name) > 256 || strings.ContainsAny(option.Name, "=\r\n\x00") {
			return fmt.Errorf("options[%d] has an invalid name", index)
		}
		if _, duplicate := optionNames[option.Name]; duplicate {
			return fmt.Errorf("option names must be unique")
		}
		optionNames[option.Name] = struct{}{}
		if len(option.Value) > 4096 || strings.ContainsAny(option.Value, "\r\n\x00") {
			return fmt.Errorf("options[%d] has an invalid value", index)
		}
	}
	return nil
}

func (c RoleCredentials) Validate(cfg *Backup) error {
	if c.Targets == nil || len(c.Targets) != len(cfg.Targets) {
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

func (n Notifier) validateReference() error {
	if !namePattern.MatchString(n.Name) {
		return fmt.Errorf("invalid name %q", n.Name)
	}
	if n.Type != "wechat" && n.Type != "bark" {
		return fmt.Errorf("unsupported type %q", n.Type)
	}
	if n.WebhookURL != "" || n.DeviceToken != "" || n.AuthKey != "" || n.KeyID != "" || n.TeamID != "" || n.Topic != "" || n.Sound != "" {
		return fmt.Errorf("secret fields belong in notifier credentials")
	}
	return nil
}

func (c NotifierCredentials) Resolve(cfg *Backup) ([]Notifier, error) {
	if c.Notifiers == nil || len(c.Notifiers) != len(cfg.Notifiers) {
		return nil, fmt.Errorf("notifier key set must exactly match backup config")
	}
	resolved := make([]Notifier, 0, len(cfg.Notifiers))
	for _, reference := range cfg.Notifiers {
		secrets, exists := c.Notifiers[reference.Name]
		if !exists {
			return nil, fmt.Errorf("notifier key set must exactly match backup config")
		}
		notifier := reference
		if err := secrets.apply(&notifier); err != nil {
			return nil, fmt.Errorf("notifier %s: %w", reference.Name, err)
		}
		resolved = append(resolved, notifier)
	}
	return resolved, nil
}

func (secrets NotifierSecrets) apply(notifier *Notifier) error {
	switch notifier.Type {
	case "wechat":
		if secrets.WebhookURL == nil || secrets.DeviceToken != nil || secrets.AuthKey != nil || secrets.KeyID != nil ||
			secrets.TeamID != nil || secrets.Topic != nil || secrets.Sound != nil {
			return fmt.Errorf("fields are invalid for WeChat")
		}
		if _, err := parseHTTPSURL(*secrets.WebhookURL); err != nil {
			return fmt.Errorf("webhook_url must be an HTTPS URL without user info or fragment")
		}
		notifier.WebhookURL = *secrets.WebhookURL
	case "bark":
		if secrets.WebhookURL != nil || secrets.DeviceToken == nil || secrets.AuthKey == nil || secrets.KeyID == nil ||
			secrets.TeamID == nil || secrets.Topic == nil {
			return fmt.Errorf("bark fields are incomplete or invalid")
		}
		notifier.DeviceToken = *secrets.DeviceToken
		notifier.AuthKey = *secrets.AuthKey
		notifier.KeyID = *secrets.KeyID
		notifier.TeamID = *secrets.TeamID
		notifier.Topic = *secrets.Topic
		if secrets.Sound != nil {
			notifier.Sound = *secrets.Sound
		}
		if notifier.Sound == "" {
			notifier.Sound = "1107"
		}
		if err := validateBark(notifier); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported type %q", notifier.Type)
	}
	return nil
}

func validateBark(notifier *Notifier) error {
	if len(notifier.DeviceToken) != 64 || strings.ToLower(notifier.DeviceToken) != notifier.DeviceToken {
		return fmt.Errorf("device_token must be 64 lowercase hexadecimal characters")
	}
	if _, err := hex.DecodeString(notifier.DeviceToken); err != nil {
		return fmt.Errorf("device_token must be 64 lowercase hexadecimal characters")
	}
	if len(notifier.AuthKey) < 1 || len(notifier.AuthKey) > 16<<10 || strings.IndexByte(notifier.AuthKey, 0) >= 0 {
		return fmt.Errorf("auth_key is invalid")
	}
	block, rest := pem.Decode([]byte(notifier.AuthKey))
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return fmt.Errorf("auth_key must contain one PKCS#8 private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(*ecdsa.PrivateKey)
	if err != nil || !ok || key.Curve != elliptic.P256() {
		return fmt.Errorf("auth_key must be a P-256 PKCS#8 private key")
	}
	if _, err := key.ECDH(); err != nil {
		return fmt.Errorf("auth_key must be a P-256 PKCS#8 private key")
	}
	for _, field := range []struct {
		name  string
		value string
	}{{"key_id", notifier.KeyID}, {"team_id", notifier.TeamID}, {"topic", notifier.Topic}} {
		if len(field.value) < 2 || len(field.value) > 255 || strings.ContainsAny(field.value, "\r\n\x00") {
			return fmt.Errorf("%s is invalid", field.name)
		}
	}
	if len(notifier.Sound) > 255 || strings.ContainsAny(notifier.Sound, "\r\n\x00") {
		return fmt.Errorf("sound is invalid")
	}
	return nil
}

func parseHTTPSURL(raw string) (*url.URL, error) {
	if len(raw) < 1 || len(raw) > 4096 || strings.ContainsAny(raw, "\r\n\x00") {
		return nil, fmt.Errorf("URL is invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path == "" ||
		parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" {
		return nil, fmt.Errorf("URL is invalid")
	}
	return parsed, nil
}

func LoadDeployment() (Deployment, error) {
	values := make(map[string]string, len(deploymentEnvironmentNames))
	for _, name := range deploymentEnvironmentNames {
		value, present := os.LookupEnv(name)
		if !present || value == "" {
			return Deployment{}, fmt.Errorf("%s is required", name)
		}
		values[name] = value
	}
	return decodeDeploymentEnvironment(values)
}

var deploymentEnvironmentNames = [...]string{
	"DEPLOYMENT_ROOT", "RUNTIME_UID", "RUNTIME_GID", "LISTEN_PORT", "DRILL_PORT",
}

func decodeDeploymentEnvironment(values map[string]string) (Deployment, error) {
	root := values["DEPLOYMENT_ROOT"]
	if root == "" || root == string(filepath.Separator) || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return Deployment{}, fmt.Errorf("DEPLOYMENT_ROOT is invalid")
	}
	deployment := Deployment{Root: root}
	uid, err := parseCanonicalUint(values["RUNTIME_UID"], 1, 4294967294)
	if err != nil {
		return Deployment{}, fmt.Errorf("RUNTIME_UID is invalid")
	}
	gid, err := parseCanonicalUint(values["RUNTIME_GID"], 1, 4294967294)
	if err != nil {
		return Deployment{}, fmt.Errorf("RUNTIME_GID is invalid")
	}
	listen, err := parseCanonicalUint(values["LISTEN_PORT"], 1024, 65535)
	if err != nil {
		return Deployment{}, fmt.Errorf("LISTEN_PORT is invalid")
	}
	drill, err := parseCanonicalUint(values["DRILL_PORT"], 1024, 65535)
	if err != nil || drill == listen {
		return Deployment{}, fmt.Errorf("DRILL_PORT is invalid or duplicates LISTEN_PORT")
	}
	deployment.RuntimeUID = uint32(uid)
	deployment.RuntimeGID = uint32(gid)
	deployment.ListenPort = uint16(listen)
	deployment.DrillPort = uint16(drill)
	if deployment.RuntimeUID != uint32(os.Geteuid()) || deployment.RuntimeGID != uint32(os.Getegid()) {
		return Deployment{}, fmt.Errorf("runtime identity does not match the validating process")
	}
	return deployment, nil
}

func parseCanonicalUint(raw string, minimum, maximum uint64) (uint64, error) {
	if !canonicalDecimal.MatchString(raw) {
		return 0, fmt.Errorf("not canonical decimal")
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("out of range")
	}
	return value, nil
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
		if strings.HasSuffix(entry.Name(), ".example") {
			continue
		}
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
