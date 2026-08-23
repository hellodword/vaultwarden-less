package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hellodword/vaultwarden-less/internal/version"
)

const maxConfigBytes = 1 << 20

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

type Proxy struct {
	SchemaVersion                 int      `json:"schema_version"`
	ListenAddress                 string   `json:"listen_address"`
	UpstreamURL                   string   `json:"upstream_url"`
	EventsURL                     string   `json:"events_url"`
	TrustedProxyCIDRs             []string `json:"trusted_proxy_cidrs"`
	MaxRequestBytes               int64    `json:"max_request_bytes"`
	ReadHeaderTimeout             Duration `json:"read_header_timeout"`
	ReadTimeout                   Duration `json:"read_timeout"`
	WriteTimeout                  Duration `json:"write_timeout"`
	IdleTimeout                   Duration `json:"idle_timeout"`
	UpstreamResponseHeaderTimeout Duration `json:"upstream_response_header_timeout"`
	EventTimeout                  Duration `json:"event_timeout"`
	ShutdownTimeout               Duration `json:"shutdown_timeout"`

	Upstream *url.URL       `json:"-"`
	Events   *url.URL       `json:"-"`
	Trusted  []netip.Prefix `json:"-"`
}

type Backup struct {
	SchemaVersion      int        `json:"schema_version"`
	ListenAddress      string     `json:"listen_address"`
	DataDir            string     `json:"data_dir"`
	StateDir           string     `json:"state_dir"`
	StagingDir         string     `json:"staging_dir"`
	CacheDir           string     `json:"cache_dir"`
	ResticBinary       string     `json:"restic_binary"`
	Debounce           Duration   `json:"debounce"`
	MaxWait            Duration   `json:"max_wait"`
	MinInterval        Duration   `json:"min_interval"`
	SampleInterval     Duration   `json:"sample_interval"`
	MissingRetry       Duration   `json:"missing_retry"`
	TargetTimeout      Duration   `json:"target_timeout"`
	MaintenanceTimeout Duration   `json:"maintenance_timeout"`
	ShutdownTimeout    Duration   `json:"shutdown_timeout"`
	EventMaxBytes      int64      `json:"event_max_bytes"`
	EventRatePerSecond float64    `json:"event_rate_per_second"`
	EventBurst         int        `json:"event_burst"`
	Targets            []Target   `json:"targets"`
	Notifiers          []Notifier `json:"notifiers"`
}

type Target struct {
	Name                   string            `json:"name"`
	Backend                string            `json:"backend"`
	RepositoryFile         string            `json:"repository_file"`
	CACertificateFile      string            `json:"ca_certificate_file,omitempty"`
	Options                map[string]string `json:"options,omitempty"`
	BackupCredentials      Credentials       `json:"backup_credentials"`
	MaintenanceCredentials *Credentials      `json:"maintenance_credentials,omitempty"`
}

type Credentials struct {
	PasswordFile     string            `json:"password_file"`
	EnvironmentFiles map[string]string `json:"environment_files,omitempty"`
	EnvironmentPaths map[string]string `json:"environment_paths,omitempty"`
}

type Notifier struct {
	Name             string            `json:"name"`
	Type             string            `json:"type"`
	Timeout          Duration          `json:"timeout"`
	URL              string            `json:"url,omitempty"`
	URLFile          string            `json:"url_file,omitempty"`
	HeadersFromFiles map[string]string `json:"headers_from_files,omitempty"`
	Endpoint         string            `json:"endpoint,omitempty"`
	DeviceTokenFile  string            `json:"device_token_file,omitempty"`
	AuthKeyFile      string            `json:"auth_key_file,omitempty"`
	KeyID            string            `json:"key_id,omitempty"`
	TeamID           string            `json:"team_id,omitempty"`
	Topic            string            `json:"topic,omitempty"`
	Sound            string            `json:"sound,omitempty"`
}

func LoadProxy(path string) (*Proxy, error) {
	var cfg Proxy
	if err := decodeFile(path, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate proxy config: %w", err)
	}
	return &cfg, nil
}

func LoadBackup(path string) (*Backup, error) {
	var cfg Backup
	if err := decodeFile(path, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate backup config: %w", err)
	}
	return &cfg, nil
}

func decodeFile(path string, dst any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxConfigBytes {
		return fmt.Errorf("config must be a regular file no larger than %d bytes", maxConfigBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, maxConfigBytes+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode config: multiple JSON values")
		}
		return fmt.Errorf("decode config trailer: %w", err)
	}
	return nil
}

func DecodeProxy(data []byte) (*Proxy, error) {
	var cfg Proxy
	if err := decodeBytes(data, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func DecodeBackup(data []byte) (*Backup, error) {
	var cfg Backup
	if err := decodeBytes(data, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func decodeBytes(data []byte, dst any) error {
	if len(data) > maxConfigBytes {
		return fmt.Errorf("config exceeds %d bytes", maxConfigBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}

func (c *Proxy) Validate() error {
	if c.SchemaVersion != version.SchemaVersion {
		return fmt.Errorf("schema_version must be %d", version.SchemaVersion)
	}
	if err := validListen(c.ListenAddress); err != nil {
		return fmt.Errorf("listen_address: %w", err)
	}
	upstream, err := parseHTTPURL(c.UpstreamURL, false)
	if err != nil {
		return fmt.Errorf("upstream_url: %w", err)
	}
	if upstream.Path != "" && upstream.Path != "/" || upstream.RawQuery != "" {
		return fmt.Errorf("upstream_url must not contain a path or query")
	}
	events, err := parseHTTPURL(c.EventsURL, false)
	if err != nil {
		return fmt.Errorf("events_url: %w", err)
	}
	if events.Path != "/v1/events" || events.RawQuery != "" {
		return fmt.Errorf("events_url path must be /v1/events")
	}
	if len(c.TrustedProxyCIDRs) == 0 {
		return fmt.Errorf("trusted_proxy_cidrs must not be empty")
	}
	trusted := make([]netip.Prefix, 0, len(c.TrustedProxyCIDRs))
	for _, raw := range c.TrustedProxyCIDRs {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.String() != raw || prefix.Masked() != prefix {
			return fmt.Errorf("trusted_proxy_cidrs contains invalid canonical CIDR %q", raw)
		}
		trusted = append(trusted, prefix)
	}
	if c.MaxRequestBytes != 600<<20 {
		return fmt.Errorf("max_request_bytes must be 629145600")
	}
	durations := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"read_header_timeout", c.ReadHeaderTimeout.Duration, 10 * time.Second},
		{"read_timeout", c.ReadTimeout.Duration, 30 * time.Minute},
		{"write_timeout", c.WriteTimeout.Duration, 30 * time.Minute},
		{"idle_timeout", c.IdleTimeout.Duration, 2 * time.Minute},
		{"upstream_response_header_timeout", c.UpstreamResponseHeaderTimeout.Duration, 5 * time.Minute},
		{"shutdown_timeout", c.ShutdownTimeout.Duration, 2 * time.Minute},
	}
	for _, item := range durations {
		if item.got != item.want {
			return fmt.Errorf("%s must be %s", item.name, item.want)
		}
	}
	if c.EventTimeout.Duration < time.Second || c.EventTimeout.Duration > 10*time.Second {
		return fmt.Errorf("event_timeout must be between 1s and 10s")
	}
	c.Upstream, c.Events, c.Trusted = upstream, events, trusted
	return nil
}

func (c *Backup) Validate() error {
	if c.SchemaVersion != version.SchemaVersion {
		return fmt.Errorf("schema_version must be %d", version.SchemaVersion)
	}
	if err := validListen(c.ListenAddress); err != nil {
		return fmt.Errorf("listen_address: %w", err)
	}
	paths := []struct {
		name string
		path string
	}{
		{"data_dir", c.DataDir}, {"state_dir", c.StateDir}, {"staging_dir", c.StagingDir}, {"cache_dir", c.CacheDir}, {"restic_binary", c.ResticBinary},
	}
	for _, item := range paths {
		if err := cleanAbsolutePath(item.path); err != nil {
			return fmt.Errorf("%s: %w", item.name, err)
		}
	}
	for _, other := range []struct {
		name string
		path string
	}{{"state_dir", c.StateDir}, {"staging_dir", c.StagingDir}, {"cache_dir", c.CacheDir}} {
		if pathsOverlap(c.DataDir, other.path) {
			return fmt.Errorf("data_dir and %s must not overlap", other.name)
		}
	}
	if pathsOverlap(c.StateDir, c.StagingDir) || pathsOverlap(c.StateDir, c.CacheDir) || pathsOverlap(c.StagingDir, c.CacheDir) {
		return fmt.Errorf("state_dir, staging_dir, and cache_dir must not overlap")
	}
	wants := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"debounce", c.Debounce.Duration, 30 * time.Second},
		{"max_wait", c.MaxWait.Duration, 2 * time.Minute},
		{"min_interval", c.MinInterval.Duration, 5 * time.Minute},
		{"sample_interval", c.SampleInterval.Duration, 15 * time.Minute},
		{"missing_retry", c.MissingRetry.Duration, 30 * time.Second},
		{"shutdown_timeout", c.ShutdownTimeout.Duration, 2 * time.Minute},
	}
	for _, item := range wants {
		if item.got != item.want {
			return fmt.Errorf("%s must be %s", item.name, item.want)
		}
	}
	if c.TargetTimeout.Duration < time.Minute || c.TargetTimeout.Duration > 2*time.Hour {
		return fmt.Errorf("target_timeout must be between 1m and 2h")
	}
	if c.MaintenanceTimeout.Duration < time.Minute || c.MaintenanceTimeout.Duration > 24*time.Hour {
		return fmt.Errorf("maintenance_timeout must be between 1m and 24h")
	}
	if c.EventMaxBytes < 16 || c.EventMaxBytes > 4096 {
		return fmt.Errorf("event_max_bytes must be between 16 and 4096")
	}
	if c.EventRatePerSecond <= 0 || c.EventRatePerSecond > 1000 || c.EventBurst < 1 || c.EventBurst > 10000 {
		return fmt.Errorf("event rate or burst is outside supported bounds")
	}
	if len(c.Targets) == 0 {
		return fmt.Errorf("targets must not be empty")
	}
	if len(c.Targets) > 32 {
		return fmt.Errorf("targets must not contain more than 32 entries")
	}
	targetNames := map[string]struct{}{}
	for i := range c.Targets {
		if err := c.Targets[i].validate(); err != nil {
			return fmt.Errorf("targets[%d]: %w", i, err)
		}
		if _, exists := targetNames[c.Targets[i].Name]; exists {
			return fmt.Errorf("duplicate target name %q", c.Targets[i].Name)
		}
		targetNames[c.Targets[i].Name] = struct{}{}
	}
	notifierNames := map[string]struct{}{}
	if len(c.Notifiers) > 16 {
		return fmt.Errorf("notifiers must not contain more than 16 entries")
	}
	for i := range c.Notifiers {
		if err := c.Notifiers[i].validate(); err != nil {
			return fmt.Errorf("notifiers[%d]: %w", i, err)
		}
		if _, exists := notifierNames[c.Notifiers[i].Name]; exists {
			return fmt.Errorf("duplicate notifier name %q", c.Notifiers[i].Name)
		}
		notifierNames[c.Notifiers[i].Name] = struct{}{}
	}
	return nil
}

func (t *Target) validate() error {
	if !namePattern.MatchString(t.Name) {
		return fmt.Errorf("invalid name %q", t.Name)
	}
	if !slices.Contains([]string{"local", "rest", "s3", "b2", "azure", "gcs", "swift"}, t.Backend) {
		return fmt.Errorf("unsupported backend %q", t.Backend)
	}
	if err := cleanAbsolutePath(t.RepositoryFile); err != nil {
		return fmt.Errorf("repository_file: %w", err)
	}
	if t.CACertificateFile != "" {
		if err := cleanAbsolutePath(t.CACertificateFile); err != nil {
			return fmt.Errorf("ca_certificate_file: %w", err)
		}
	}
	if err := t.validateOptions(); err != nil {
		return err
	}
	if err := t.BackupCredentials.validate(); err != nil {
		return fmt.Errorf("backup_credentials: %w", err)
	}
	if t.MaintenanceCredentials != nil {
		if err := t.MaintenanceCredentials.validate(); err != nil {
			return fmt.Errorf("maintenance_credentials: %w", err)
		}
	}
	return nil
}

var allowedValueEnvironment = map[string]struct{}{
	"AWS_ACCESS_KEY_ID": {}, "AWS_SECRET_ACCESS_KEY": {}, "AWS_SESSION_TOKEN": {}, "AWS_DEFAULT_REGION": {},
	"B2_ACCOUNT_ID": {}, "B2_ACCOUNT_KEY": {},
	"AZURE_ACCOUNT_NAME": {}, "AZURE_ACCOUNT_KEY": {}, "AZURE_ACCOUNT_SAS": {}, "AZURE_ENDPOINT_SUFFIX": {},
	"GOOGLE_PROJECT_ID":    {},
	"RESTIC_REST_USERNAME": {}, "RESTIC_REST_PASSWORD": {},
	"ST_AUTH": {}, "ST_USER": {}, "ST_KEY": {},
	"OS_AUTH_URL": {}, "OS_REGION_NAME": {}, "OS_USERNAME": {}, "OS_USER_ID": {}, "OS_PASSWORD": {},
	"OS_TENANT_ID": {}, "OS_TENANT_NAME": {}, "OS_USER_DOMAIN_NAME": {}, "OS_USER_DOMAIN_ID": {},
	"OS_PROJECT_NAME": {}, "OS_PROJECT_DOMAIN_NAME": {}, "OS_PROJECT_DOMAIN_ID": {}, "OS_TRUST_ID": {},
	"OS_APPLICATION_CREDENTIAL_ID": {}, "OS_APPLICATION_CREDENTIAL_NAME": {}, "OS_APPLICATION_CREDENTIAL_SECRET": {},
	"OS_STORAGE_URL": {}, "OS_AUTH_TOKEN": {}, "SWIFT_DEFAULT_CONTAINER_POLICY": {},
}

var allowedPathEnvironment = map[string]struct{}{
	"GOOGLE_APPLICATION_CREDENTIALS": {},
}

func (c Credentials) validate() error {
	if err := cleanAbsolutePath(c.PasswordFile); err != nil {
		return fmt.Errorf("password_file: %w", err)
	}
	seen := map[string]struct{}{}
	for name, path := range c.EnvironmentFiles {
		if _, ok := allowedValueEnvironment[name]; !ok {
			return fmt.Errorf("environment_files contains unsupported variable %q", name)
		}
		if err := cleanAbsolutePath(path); err != nil {
			return fmt.Errorf("environment_files[%s]: %w", name, err)
		}
		seen[name] = struct{}{}
	}
	for name, path := range c.EnvironmentPaths {
		if _, ok := allowedPathEnvironment[name]; !ok {
			return fmt.Errorf("environment_paths contains unsupported variable %q", name)
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("environment variable %q is configured twice", name)
		}
		if err := cleanAbsolutePath(path); err != nil {
			return fmt.Errorf("environment_paths[%s]: %w", name, err)
		}
	}
	return nil
}

func (t Target) validateOptions() error {
	for name, value := range t.Options {
		if t.Backend != "s3" {
			return fmt.Errorf("options are not supported for backend %q", t.Backend)
		}
		switch name {
		case "s3.bucket-lookup":
			if !slices.Contains([]string{"auto", "dns", "path"}, value) {
				return fmt.Errorf("options[%s] must be auto, dns, or path", name)
			}
		case "s3.region":
			if len(value) < 1 || len(value) > 63 || !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`).MatchString(value) {
				return fmt.Errorf("options[%s] is invalid", name)
			}
		case "s3.list-objects-v1":
			if value != "true" && value != "false" {
				return fmt.Errorf("options[%s] must be true or false", name)
			}
		default:
			return fmt.Errorf("unsupported Restic option %q", name)
		}
	}
	return nil
}

func (n *Notifier) validate() error {
	if !namePattern.MatchString(n.Name) {
		return fmt.Errorf("invalid name %q", n.Name)
	}
	if n.Timeout.Duration < time.Second || n.Timeout.Duration > time.Minute {
		return fmt.Errorf("timeout must be between 1s and 1m")
	}
	switch n.Type {
	case "webhook":
		if n.URL == "" || n.URLFile != "" || n.Endpoint != "" || n.DeviceTokenFile != "" || n.AuthKeyFile != "" || n.KeyID != "" || n.TeamID != "" || n.Topic != "" || n.Sound != "" {
			return fmt.Errorf("webhook fields are invalid for this notifier type")
		}
		parsed, err := parseHTTPURL(n.URL, true)
		if err != nil || parsed.RawQuery != "" {
			return fmt.Errorf("url must be an HTTPS URL without query credentials")
		}
		canonicalHeaders := make(map[string]struct{}, len(n.HeadersFromFiles))
		for header, path := range n.HeadersFromFiles {
			if !validSecretHeader(header) {
				return fmt.Errorf("unsupported secret header %q", header)
			}
			canonical := strings.ToLower(header)
			if _, duplicate := canonicalHeaders[canonical]; duplicate {
				return fmt.Errorf("duplicate secret header %q", header)
			}
			canonicalHeaders[canonical] = struct{}{}
			if err := cleanAbsolutePath(path); err != nil {
				return fmt.Errorf("headers_from_files[%s]: %w", header, err)
			}
		}
	case "wechat":
		if n.URL != "" || n.URLFile == "" || len(n.HeadersFromFiles) != 0 || n.Endpoint != "" || n.DeviceTokenFile != "" || n.AuthKeyFile != "" || n.KeyID != "" || n.TeamID != "" || n.Topic != "" || n.Sound != "" {
			return fmt.Errorf("wechat fields are invalid for this notifier type")
		}
		if err := cleanAbsolutePath(n.URLFile); err != nil {
			return fmt.Errorf("url_file: %w", err)
		}
	case "bark":
		if n.URL != "" || n.URLFile != "" || len(n.HeadersFromFiles) != 0 || n.Endpoint == "" || n.DeviceTokenFile == "" || n.AuthKeyFile == "" || n.KeyID == "" || n.TeamID == "" || n.Topic == "" {
			return fmt.Errorf("bark fields are incomplete or invalid")
		}
		endpoint, err := parseHTTPURL(n.Endpoint, true)
		if err != nil || endpoint.Path != "" && endpoint.Path != "/" || endpoint.RawQuery != "" || endpoint.Port() != "" ||
			!slices.Contains([]string{"api.push.apple.com", "api.sandbox.push.apple.com"}, endpoint.Hostname()) {
			return fmt.Errorf("endpoint must be an official APNs HTTPS origin")
		}
		for label, path := range map[string]string{"device_token_file": n.DeviceTokenFile, "auth_key_file": n.AuthKeyFile} {
			if err := cleanAbsolutePath(path); err != nil {
				return fmt.Errorf("%s: %w", label, err)
			}
		}
		for label, value := range map[string]string{"key_id": n.KeyID, "team_id": n.TeamID, "topic": n.Topic} {
			if len(value) < 2 || len(value) > 255 || strings.ContainsAny(value, "\r\n") {
				return fmt.Errorf("%s is invalid", label)
			}
		}
		if len(n.Sound) > 255 || strings.ContainsAny(n.Sound, "\r\n") {
			return fmt.Errorf("sound is invalid")
		}
	default:
		return fmt.Errorf("unsupported type %q", n.Type)
	}
	return nil
}

func validListen(value string) error {
	host, port, err := net.SplitHostPort(value)
	if err != nil || port == "" {
		return fmt.Errorf("must be host:port")
	}
	if host != "" {
		if _, err := netip.ParseAddr(host); err != nil {
			return fmt.Errorf("host must be an IP address")
		}
	}
	numericPort, err := strconv.Atoi(port)
	if err != nil || numericPort < 1 || numericPort > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}

func parseHTTPURL(raw string, httpsOnly bool) (*url.URL, error) {
	if len(raw) < 1 || len(raw) > 4096 || strings.ContainsAny(raw, "\r\n\x00") {
		return nil, fmt.Errorf("URL length or characters are invalid")
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, fmt.Errorf("must be an absolute URL without user info or fragment")
	}
	if httpsOnly && parsed.Scheme != "https" {
		return nil, fmt.Errorf("scheme must be https")
	}
	if !httpsOnly && parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("scheme must be http or https")
	}
	return parsed, nil
}

func cleanAbsolutePath(path string) error {
	if path == "" || path == string(filepath.Separator) || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.IndexByte(path, 0) >= 0 {
		return fmt.Errorf("must be a clean absolute non-root path")
	}
	return nil
}

func pathsOverlap(left, right string) bool {
	rel, err := filepath.Rel(left, right)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	rel, err = filepath.Rel(right, left)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func validSecretHeader(name string) bool {
	canonical := strings.ToLower(name)
	return canonical == "authorization" || canonical == "x-api-key" || strings.HasPrefix(canonical, "x-vaultwarden-less-")
}
