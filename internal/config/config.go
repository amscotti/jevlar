// Package config loads, expands and validates Jevlar's TOML configuration.
//
// The package owns the configuration contract: strict TOML decoding (unknown
// fields are rejected), ${VAR} environment expansion with field-path errors,
// outbound key file resolution, and semantic validation that aggregates every
// rule violation into one report. Configuration is fail-closed: a caller must
// not bind a listener or start serving until Load and Validate have both
// succeeded. The package imports no other Jevlar package; it may import only
// the standard library and go-toml/v2.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

// ConfigError is one configuration rule violation, anchored to the field it
// concerns. Field is a dotted, index-aware path into the TOML document:
// array elements are addressed as "providers[1].name" and values inside a
// map field as models."jev-latest".targets[0].provider (map keys are
// TOML-quoted); it is empty for whole-file errors such as a missing file.
// Error renders as "field: msg", or just "msg" when Field is empty.
//
// Err optionally carries the underlying error (a file-read or decode
// failure); Unwrap exposes it so errors.Is and errors.As reach the cause.
type ConfigError struct { //nolint:revive // the exported name is the frozen public error type; the config.ConfigError stutter is accepted
	Field string
	Msg   string
	Err   error
}

// Error implements the error interface.
func (e ConfigError) Error() string {
	if e.Field == "" {
		return e.Msg
	}
	return e.Field + ": " + e.Msg
}

// Unwrap returns the underlying cause, if one was recorded.
func (e ConfigError) Unwrap() error {
	return e.Err
}

// ValidationErrors is a list of configuration errors rendered one per line.
// It unwraps into its parts so callers can extract individual ConfigErrors
// with errors.As.
type ValidationErrors []ConfigError

// Error implements the error interface, one violation per line.
func (e ValidationErrors) Error() string {
	parts := make([]string, len(e))
	for i, ce := range e {
		parts[i] = ce.Error()
	}
	return strings.Join(parts, "\n")
}

// Unwrap returns the individual errors so errors.As reaches each part.
func (e ValidationErrors) Unwrap() []error {
	conv := make([]error, len(e))
	for i, ce := range e {
		conv[i] = ce
	}
	return conv
}

// joined turns a slice of ConfigErrors into a single error, or nil when the
// slice is empty so callers can test err != nil.
func joined(errs []ConfigError) error {
	if len(errs) == 0 {
		return nil
	}
	return ValidationErrors(errs)
}

// Warning is a non-fatal configuration finding: a problem an operator
// should see but that does not stop the boot. Load and Parse collect
// warnings while they work; the caller logs them. Warnings are never part
// of the configuration's canonical form.
type Warning struct {
	// Field anchors the warning to a configuration path; empty for
	// whole-file findings such as the configuration file's own
	// permissions.
	Field string
	// Msg describes the finding.
	Msg string
}

// String renders the warning as "field: msg", or just "msg".
func (w Warning) String() string {
	if w.Field == "" {
		return w.Msg
	}
	return w.Field + ": " + w.Msg
}

// Config is a decoded, expanded and defaulted configuration: the complete
// v1 schema. Unknown keys anywhere in the document are rejected at decode
// time, so a Config always mirrors the schema exactly.
//
// Map-keyed tables (accounts and models) have their values expanded like
// every other string; the map KEYS themselves are never expanded — an
// account literally named "${X}" is an account named "${X}" — because keys
// name entities, they do not carry data.
//
// Warnings carries load-time findings (file permissions, literal key
// values); it is populated by the loader, never by the document, and is
// excluded from the canonical form used for config hashing.
type Config struct {
	// ConfigVersion is the configuration schema version; only 1 is accepted.
	ConfigVersion int `toml:"config_version"`
	// Server holds the listener and process settings.
	Server Server `toml:"server"`
	// Security holds file-permission policy for secrets.
	Security Security `toml:"security"`
	// Usage holds the persistent usage-rollup settings.
	Usage Usage `toml:"usage"`
	// Auth holds the inbound client keys.
	Auth Auth `toml:"auth"`
	// Accounts maps an account name to its provider-side rate limits.
	// Provider limits are per account, not per key.
	Accounts map[string]Account `toml:"accounts"`
	// Providers is the list of upstream providers; validation requires at
	// least one and unique names. The TOML form is an array of tables
	// ([[providers]]); a single [providers] table is leniently accepted as
	// a one-element list, mirroring the TOML decoder.
	Providers []Provider `toml:"providers"`
	// Models maps a model alias to its routing configuration: an ordered
	// target list plus selection policy.
	Models map[string]Model `toml:"models"`
	// Breaker holds the provider circuit-breaker settings.
	Breaker Breaker `toml:"breaker"`
	// Warnings holds non-fatal load-time findings. It is never decoded
	// from the document.
	Warnings []Warning `toml:"-"`
}

// Server holds the settings for the main listener, the admin listener and
// the process.
type Server struct {
	// Listen is the host:port the main listener binds.
	Listen string `toml:"listen"`
	// AdminListen is the host:port the admin listener binds. Empty (the
	// default) disables the admin plane entirely: no listener, no
	// /admin/status, no /admin/reload. When set without an admin_token it
	// must be loopback.
	AdminListen string `toml:"admin_listen"`
	// AdminToken protects the admin plane when set. Empty means the admin
	// listener must be loopback (enforced both at validation and in the
	// handler, which rejects non-loopback remote addresses with 403).
	// When set it must be at least 32 characters and is compared with
	// callers' bearer tokens in constant time.
	AdminToken string `toml:"admin_token"`
	// RequestTimeoutMs bounds one client request across every attempt;
	// 0 means "use the default" (DefaultRequestTimeoutMs).
	RequestTimeoutMs int `toml:"request_timeout_ms"`
	// ShutdownGraceMs bounds graceful shutdown after SIGTERM or SIGINT; 0
	// means "use the default" (DefaultShutdownGraceMs) and it must be
	// between 0 and MaxShutdownGraceMs.
	ShutdownGraceMs int `toml:"shutdown_grace_ms"`
	// ReadHeaderTimeoutMs bounds how long reading a request's headers may
	// take; 0 means "use the default" (DefaultReadHeaderTimeoutMs).
	ReadHeaderTimeoutMs int `toml:"read_header_timeout_ms"`
	// MaxBodyBytes caps the accepted request body; 0 means "use the
	// default" (DefaultMaxBodyBytes).
	MaxBodyBytes int64 `toml:"max_body_bytes"`
	// MaxResponseBytes caps the buffered upstream response; 0 means "use
	// the default" (DefaultMaxResponseBytes).
	MaxResponseBytes int64 `toml:"max_response_bytes"`
	// MaxInflight caps concurrently admitted requests; 0 means "use the
	// default" (DefaultMaxInflight).
	MaxInflight int `toml:"max_inflight"`
	// LogLevel is one of debug, info, warn, error; empty means the
	// default (DefaultLogLevel).
	LogLevel string `toml:"log_level"`
	// LogBodies turns on request/response body logging; it requires
	// log_level = "debug" and is a debugging switch, not an operations
	// mode.
	LogBodies bool `toml:"log_bodies"`
}

// Default values for server settings, applied when the configuration
// leaves a field unset (its zero value).
const (
	// DefaultRequestTimeoutMs is applied when request_timeout_ms is unset.
	DefaultRequestTimeoutMs = 30_000
	// DefaultShutdownGraceMs is applied when shutdown_grace_ms is unset.
	DefaultShutdownGraceMs = 10_000
	// MaxShutdownGraceMs is the largest accepted server.shutdown_grace_ms;
	// ten minutes of graceful drain is the ceiling beyond which the
	// operator should fix the client instead.
	MaxShutdownGraceMs = 600_000
	// DefaultReadHeaderTimeoutMs is applied when read_header_timeout_ms is
	// unset.
	DefaultReadHeaderTimeoutMs = 5_000
	// DefaultMaxBodyBytes is applied when max_body_bytes is unset (256 KiB).
	DefaultMaxBodyBytes = 262_144
	// DefaultMaxResponseBytes is applied when max_response_bytes is unset
	// (1 MiB).
	DefaultMaxResponseBytes = 1_048_576
	// DefaultMaxInflight is applied when max_inflight is unset.
	DefaultMaxInflight = 4_096
	// MaxProviderTimeoutMs is the largest accepted providers[].timeout_ms.
	MaxProviderTimeoutMs = 120_000
	// DefaultLogLevel is applied when log_level is unset.
	DefaultLogLevel = "info"
)

// Security holds the policy for secret-bearing files.
type Security struct {
	// StrictFilePermissions turns group/world-accessible secret files (the
	// configuration file itself and every api_keys[].value_file) from a
	// warning into a load error. It is off by default because container
	// runtimes mount secrets at 0444/0644.
	StrictFilePermissions bool `toml:"strict_file_permissions"`
}

// Usage holds the persistent usage-rollup settings. Rollups are written
// only when Enabled is true.
type Usage struct {
	// Enabled turns on durable usage rollups.
	Enabled bool `toml:"enabled"`
	// Dir is the directory rollup files are written to; when usage is
	// enabled it must exist and be writable. Empty means the default
	// (DefaultUsageDir).
	Dir string `toml:"dir"`
	// RetentionDays bounds how long rollup files are kept; 0 means the
	// default (DefaultUsageRetentionDays).
	RetentionDays int `toml:"retention_days"`
	// MaxFileMB bounds a single rollup file before rotation; 0 means the
	// default (DefaultUsageMaxFileMB).
	MaxFileMB int `toml:"max_file_mb"`
}

// Defaults for usage settings.
const (
	// DefaultUsageDir is applied when usage.dir is unset.
	DefaultUsageDir = "/var/lib/jevlar"
	// DefaultUsageRetentionDays is applied when usage.retention_days is
	// unset.
	DefaultUsageRetentionDays = 30
	// DefaultUsageMaxFileMB is applied when usage.max_file_mb is unset.
	DefaultUsageMaxFileMB = 256
)

// Auth holds the inbound client keys: the bearer tokens callers present to
// Jevlar. Only the SHA-256 hash of each token is configured (see
// `jevlar keyhash`); the token itself never appears in the file.
type Auth struct {
	// Keys is the list of inbound client keys; ids must be unique and each
	// hash must be 64 hex characters and map to exactly one id.
	Keys []AuthKey `toml:"keys"`
}

// AuthKey is one inbound client key with its quotas.
type AuthKey struct {
	// ID identifies the key in logs, metrics and /admin/status.
	ID string `toml:"id"`
	// Hash is the SHA-256 hex of the client's token.
	Hash string `toml:"hash"`
	// RPM is the client's requests-per-minute quota; 0 (absent) means
	// unlimited, negative is rejected.
	RPM int `toml:"rpm"`
	// TPM is the client's tokens-per-minute quota; 0 (absent) means
	// unlimited, negative is rejected.
	TPM int `toml:"tpm"`
}

// Account is a provider-side limit group. Providers reference an account by
// name; rate limits are per account, not per key, because a provider counts
// every key on the same account against the same quota.
type Account struct {
	// RPM is the account's requests-per-minute limit; 0 means unlimited.
	RPM int `toml:"rpm"`
	// TPM is the account's tokens-per-minute limit; 0 means unlimited.
	TPM int `toml:"tpm"`
}

// Provider is one upstream decision-model provider.
type Provider struct {
	// Name identifies the provider in logs, metrics and routing targets.
	Name string `toml:"name"`
	// BaseURL is the provider's origin: scheme://host[:port] with no path,
	// query or fragment. The request path belongs in Path. Plain http is
	// accepted only for loopback hosts.
	BaseURL string `toml:"base_url"`
	// Path is the request path used for calls to the provider; it must
	// start with "/" and contain no "..".
	Path string `toml:"path"`
	// Auth selects how Jevlar authenticates upstream: "bearer" (a key from
	// api_keys is sent as the bearer token) or "none"; empty means "none".
	Auth string `toml:"auth"`
	// Account names the account whose limits apply to this provider's
	// keys; empty means no account limits. When set it must exist in
	// [accounts].
	Account string `toml:"account"`
	// Models lists the model ids this provider serves; entries must be
	// unique. An empty list means the provider's model surface is
	// unspecified and routing targets are not checked against it.
	Models []string `toml:"models"`
	// TimeoutMs bounds one upstream attempt; 0 means the default
	// (DefaultProviderTimeoutMs). It must not exceed
	// server.request_timeout_ms.
	TimeoutMs int `toml:"timeout_ms"`
	// MaxConcurrency caps this provider's concurrent upstream requests; 0
	// means the default (DefaultProviderMaxConcurrency). Jevlar rejects
	// rather than queues when the cap is saturated.
	MaxConcurrency int `toml:"max_concurrency"`
	// APIKeys is the provider's key pool. Bearer auth requires at least
	// one key; auth "none" must define none. Each key carries its secret
	// either as an expanded value (normally a ${VAR} reference) or in a
	// file named by value_file.
	APIKeys []APIKey `toml:"api_keys"`
}

// Defaults for provider settings.
const (
	// DefaultProviderTimeoutMs is applied when providers[].timeout_ms is
	// unset.
	DefaultProviderTimeoutMs = 8_000
	// DefaultProviderMaxConcurrency is applied when
	// providers[].max_concurrency is unset.
	DefaultProviderMaxConcurrency = 64
)

// Provider auth modes.
const (
	// AuthBearer sends an api_keys entry as the upstream bearer token.
	AuthBearer = "bearer"
	// AuthNone sends no upstream credentials.
	AuthNone = "none"
)

// APIKey is one upstream key. Exactly one of Value or ValueFile must be
// set for bearer-auth providers: Value normally holds a ${VAR} reference
// expanded at load and reload; ValueFile names a file the secret is read
// from at load and reload (a trailing newline is trimmed). The file must
// be a regular file and, by default, not group/world-accessible (a
// warning; a load error under security.strict_file_permissions).
type APIKey struct {
	// ID identifies the key in logs, metrics and /admin/status.
	ID string `toml:"id"`
	// Value is the key secret, usually "${ENV_VAR}". A literal (non-empty
	// value with no ${...} reference) loads with a warning because it
	// puts the secret in the configuration file.
	Value string `toml:"value"`
	// ValueFile names a file the secret is read from.
	ValueFile string `toml:"value_file"`
	// Resolved holds the secret after expansion or file reading. It is
	// populated by the loader, never by the document, and is memory-only:
	// it is excluded from the canonical form so config hashes never
	// become verifiers of secret material.
	Resolved string `toml:"-"`
}

// Model is one model alias's routing configuration. The alias is the map
// key; targets name (provider, model) pairs tried in order.
type Model struct {
	// Targets is the ordered failover list; the first healthy target wins.
	Targets []ModelTarget `toml:"targets"`
	// Policy is "priority" (config order) or "fastest" (EWMA latency with
	// hysteresis and periodic exploration); empty means the default
	// (DefaultPolicy).
	Policy string `toml:"policy"`
	// MaxAttempts bounds how many targets one request may try; 0 means
	// the default (DefaultMaxAttempts).
	MaxAttempts int `toml:"max_attempts"`
	// EwmaAlpha is the EWMA smoothing factor for the fastest policy; 0
	// means the default (DefaultEwmaAlpha).
	EwmaAlpha float64 `toml:"ewma_alpha"`
	// HysteresisMs is the latency band inside which the fastest policy
	// does not switch leaders; 0 means the default
	// (DefaultHysteresisMs).
	HysteresisMs int `toml:"hysteresis_ms"`
	// ExploreEvery sends one request in N to the non-leading target so
	// the fastest policy can notice a leader change; 0 means the default
	// (DefaultExploreEvery). It must be at least 1 under the fastest
	// policy.
	ExploreEvery int `toml:"explore_every"`
	// Description is the human-readable text GET /v1/models reports for
	// this alias. Empty means a generated description naming the alias's
	// providers.
	Description string `toml:"description"`
	// ReleaseDate is the YYYY-MM-DD date GET /v1/models reports for this
	// alias. Empty is reported as an empty string: Jevlar does not invent
	// provider metadata.
	ReleaseDate string `toml:"release_date"`
}

// ModelTarget is one (provider, model) routing target.
type ModelTarget struct {
	// Provider names an existing provider.
	Provider string `toml:"provider"`
	// Model is the model id sent upstream; it may differ from the alias.
	Model string `toml:"model"`
}

// Defaults for model routing settings.
const (
	// DefaultPolicy is applied when models.<alias>.policy is unset.
	DefaultPolicy = "priority"
	// DefaultMaxAttempts is applied when models.<alias>.max_attempts is
	// unset.
	DefaultMaxAttempts = 3
	// DefaultEwmaAlpha is applied when models.<alias>.ewma_alpha is unset.
	DefaultEwmaAlpha = 0.2
	// DefaultHysteresisMs is applied when models.<alias>.hysteresis_ms is
	// unset.
	DefaultHysteresisMs = 15
	// DefaultExploreEvery is applied when models.<alias>.explore_every is
	// unset.
	DefaultExploreEvery = 100
)

// Routing policies.
const (
	// PolicyPriority tries targets in configuration order.
	PolicyPriority = "priority"
	// PolicyFastest tries targets by EWMA latency with hysteresis.
	PolicyFastest = "fastest"
)

// Breaker holds the provider circuit-breaker settings shared by every
// provider.
type Breaker struct {
	// Threshold is the number of consecutive provider failures that opens
	// the breaker; 0 means the default (DefaultBreakerThreshold).
	Threshold int `toml:"threshold"`
	// CooldownMs is how long an open breaker stays open before one probe
	// request is allowed; 0 means the default
	// (DefaultBreakerCooldownMs).
	CooldownMs int `toml:"cooldown_ms"`
}

// Defaults for breaker settings.
const (
	// DefaultBreakerThreshold is applied when breaker.threshold is unset.
	DefaultBreakerThreshold = 5
	// DefaultBreakerCooldownMs is applied when breaker.cooldown_ms is
	// unset.
	DefaultBreakerCooldownMs = 30_000
)

// LoadOption customises Load.
type LoadOption func(*loadOptions)

// loadOptions carries Load's overridable dependencies.
type loadOptions struct {
	lookup Lookup
}

// WithLookup overrides the environment lookup used for ${VAR} expansion, so
// tests inject fakes instead of touching the real environment. The default
// is os.LookupEnv.
func WithLookup(lookup Lookup) LoadOption {
	return func(o *loadOptions) {
		o.lookup = lookup
	}
}

// Load reads the TOML file at path, strictly decodes it, applies defaults,
// expands ${VAR} references and resolves api_keys[].value_file files. It
// returns a Config only when the file could be read, decoded, expanded and
// resolved without error; otherwise it returns nil and an error joining
// one ConfigError per problem, each carrying a field path. The path must
// name a regular file.
//
// The configuration file's own permissions are checked against the policy
// in security.strict_file_permissions (itself read from the file): a
// group/world-accessible file is a warning by default and a load error
// under strict mode. Warnings are collected on Config.Warnings for the
// caller to log.
func Load(path string, opts ...LoadOption) (*Config, error) {
	o := loadOptions{lookup: os.LookupEnv}
	for _, opt := range opts {
		opt(&o)
	}
	// The path is operator-supplied configuration on a host Jevlar already
	// trusts; it is not attacker-controlled input. Open first, then stat
	// the open file: statting a path and reading it can otherwise describe
	// two different files if the path is swapped in between (a FIFO named
	// like a regular file, for example).
	f, err := os.Open(path) //nolint:gosec // trusted operator-supplied path
	if err != nil {
		return nil, ConfigError{Msg: "read config file: " + err.Error(), Err: err}
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, ConfigError{Msg: "read config file: " + err.Error(), Err: err}
	}
	if !info.Mode().IsRegular() {
		return nil, ConfigError{Msg: "read config file: " + path + " is not a regular file"}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, ConfigError{Msg: "read config file: " + err.Error(), Err: err}
	}
	cfg, err := Parse(data, o.lookup)
	if err != nil {
		return nil, err
	}
	permErr := checkFilePermissions(path, info.Mode(), cfg.Security.StrictFilePermissions, &cfg.Warnings)
	if permErr != nil {
		return nil, permErr
	}
	return cfg, nil
}

// secretPermMask is the set of permission bits that must not be set on a
// secret-bearing file: any group or other access.
const secretPermMask = 0o077

// checkFilePermissions applies the secret-file permission policy to one
// file mode: a mode with any group or other access bit is a warning by
// default and an error under strict mode. The same message is used either
// way so operators see the same finding before and after enabling strict
// mode.
func checkFilePermissions(path string, mode os.FileMode, strict bool, warnings *[]Warning) error {
	if mode.Perm()&secretPermMask == 0 {
		return nil
	}
	perm := fmt.Sprintf("%04o", mode.Perm())
	switch {
	case strict:
		return ConfigError{
			Msg: fmt.Sprintf("%s is group/world-accessible (%s); set security.strict_file_permissions = false or restrict to 0600", path, perm),
		}
	default:
		*warnings = append(*warnings, Warning{
			Msg: fmt.Sprintf("%s is group/world-accessible (%s); restrict to 0600", path, perm),
		})
		return nil
	}
}

// utf8BOM is stripped from the head of a document before decoding; editors
// on some platforms write it transparently.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Parse strictly decodes the TOML document in data, applies defaults,
// expands ${VAR} references using lookup and resolves api_keys[].value_file
// files from disk. See Load for the error contract. Decode problems are
// found by walking the leniently parsed document against the schema
// (unknown fields, shape mismatches — see walk.go), so a document with
// several problems reports them all.
func Parse(data []byte, lookup Lookup) (*Config, error) {
	data = bytes.TrimPrefix(data, utf8BOM)
	if !utf8.Valid(data) {
		return nil, ConfigError{Msg: "file is not valid UTF-8 TOML"}
	}
	var cfg Config
	if errs := decodeErrors(data, &cfg); len(errs) > 0 {
		return nil, joined(errs)
	}
	cfg.applyDefaults()
	rawKeys := rawKeyValues(&cfg)
	if errs := expandConfig(&cfg, lookup); len(errs) > 0 {
		return nil, joined(errs)
	}
	if errs := resolveKeys(&cfg, rawKeys); len(errs) > 0 {
		return nil, joined(errs)
	}
	return &cfg, nil
}

// applyDefaults fills schema-defined defaults for unset (zero) fields.
// Zero keeps its "unset" meaning through decoding; validation rejects
// negative values that are not a legal "use the default".
func (c *Config) applyDefaults() {
	if c.Server.RequestTimeoutMs == 0 {
		c.Server.RequestTimeoutMs = DefaultRequestTimeoutMs
	}
	if c.Server.ShutdownGraceMs == 0 {
		c.Server.ShutdownGraceMs = DefaultShutdownGraceMs
	}
	if c.Server.ReadHeaderTimeoutMs == 0 {
		c.Server.ReadHeaderTimeoutMs = DefaultReadHeaderTimeoutMs
	}
	if c.Server.MaxBodyBytes == 0 {
		c.Server.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if c.Server.MaxResponseBytes == 0 {
		c.Server.MaxResponseBytes = DefaultMaxResponseBytes
	}
	if c.Server.MaxInflight == 0 {
		c.Server.MaxInflight = DefaultMaxInflight
	}
	if c.Server.LogLevel == "" {
		c.Server.LogLevel = DefaultLogLevel
	}
	if c.Usage.Dir == "" {
		c.Usage.Dir = DefaultUsageDir
	}
	if c.Usage.RetentionDays == 0 {
		c.Usage.RetentionDays = DefaultUsageRetentionDays
	}
	if c.Usage.MaxFileMB == 0 {
		c.Usage.MaxFileMB = DefaultUsageMaxFileMB
	}
	for i := range c.Providers {
		if c.Providers[i].TimeoutMs == 0 {
			c.Providers[i].TimeoutMs = DefaultProviderTimeoutMs
		}
		if c.Providers[i].MaxConcurrency == 0 {
			c.Providers[i].MaxConcurrency = DefaultProviderMaxConcurrency
		}
		if c.Providers[i].Auth == "" {
			c.Providers[i].Auth = AuthNone
		}
	}
	for name := range c.Models {
		m := c.Models[name]
		if m.Policy == "" {
			m.Policy = DefaultPolicy
		}
		if m.MaxAttempts == 0 {
			m.MaxAttempts = DefaultMaxAttempts
		}
		if m.EwmaAlpha == 0 {
			m.EwmaAlpha = DefaultEwmaAlpha
		}
		if m.HysteresisMs == 0 {
			m.HysteresisMs = DefaultHysteresisMs
		}
		if m.ExploreEvery == 0 {
			m.ExploreEvery = DefaultExploreEvery
		}
		c.Models[name] = m
	}
	if c.Breaker.Threshold == 0 {
		c.Breaker.Threshold = DefaultBreakerThreshold
	}
	if c.Breaker.CooldownMs == 0 {
		c.Breaker.CooldownMs = DefaultBreakerCooldownMs
	}
}

// rawKeyValues snapshots every api_keys[].value before expansion, so the
// literal-value warning can tell a ${VAR} reference from a literal secret
// after expansion has rewritten the fields in place.
func rawKeyValues(c *Config) [][]string {
	raw := make([][]string, len(c.Providers))
	for i := range c.Providers {
		keys := c.Providers[i].APIKeys
		raw[i] = make([]string, len(keys))
		for j := range keys {
			raw[i][j] = keys[j].Value
		}
	}
	return raw
}

// resolveKeys resolves every provider key's secret: value_file files are
// read (regular files only, one trailing newline trimmed) and checked
// against the permission policy; inline values are copied through; a key
// defining both or neither is an error; a literal (non-${...}) inline value
// warns because the secret then lives in the configuration file.
func resolveKeys(c *Config, raw [][]string) []ConfigError {
	var errs []ConfigError
	for i := range c.Providers {
		p := &c.Providers[i]
		for j := range p.APIKeys {
			k := &p.APIKeys[j]
			field := fmt.Sprintf("providers[%d].api_keys[%d]", i, j)
			switch {
			case k.Value != "" && k.ValueFile != "":
				errs = append(errs, ConfigError{
					Field: field,
					Msg:   "define either value or value_file, not both",
				})
			case k.ValueFile != "":
				value, err := readSecretFile(k.ValueFile, c.Security.StrictFilePermissions, field, &c.Warnings)
				if err != (ConfigError{}) {
					errs = append(errs, err)
					continue
				}
				k.Resolved = value
			case k.Value != "":
				k.Resolved = k.Value
				if !strings.Contains(raw[i][j], "${") {
					c.Warnings = append(c.Warnings, Warning{
						Field: field + ".value",
						Msg:   "literal key value configured; prefer value = \"${ENV_VAR}\" or value_file",
					})
				}
			default:
				errs = append(errs, ConfigError{
					Field: field,
					Msg:   "key must define value or value_file",
				})
			}
		}
	}
	return errs
}

// readSecretFile reads a key secret from a regular file (open, stat the
// open file, read; one trailing newline is trimmed because secret mounts
// end with exactly one, in either LF or CRLF form) and applies the
// file-permission policy to the same stat, so findings cannot describe a
// different file than the one read. Errors and strict-mode permission
// failures are anchored to the key's value_file field; permission warnings
// are collected on warnings. A zero ConfigError means success.
func readSecretFile(path string, strict bool, field string, warnings *[]Warning) (string, ConfigError) {
	// The path comes from operator configuration on a trusted host.
	f, err := os.Open(path) //nolint:gosec // trusted operator-supplied path
	if err != nil {
		return "", ConfigError{Field: field + ".value_file", Msg: "open: " + err.Error(), Err: err}
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", ConfigError{Field: field + ".value_file", Msg: "stat: " + err.Error(), Err: err}
	}
	if !info.Mode().IsRegular() {
		return "", ConfigError{Field: field + ".value_file", Msg: path + " is not a regular file"}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", ConfigError{Field: field + ".value_file", Msg: "read: " + err.Error(), Err: err}
	}
	if permErr := checkFilePermissions(path, info.Mode(), strict, warnings); permErr != nil {
		ce, ok := permErr.(ConfigError) //nolint:errorlint // the type is fixed here; re-wrapping loses the field anchor
		if !ok {
			return "", ConfigError{Field: field + ".value_file", Msg: permErr.Error(), Err: permErr}
		}
		ce.Field = field + ".value_file"
		return "", ce
	}
	// Trim one trailing newline in LF or CRLF form: a secret file written
	// on Windows must not resolve to a key with a stray '\r'.
	return strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r"), ConfigError{}
}

// decodeErrors reports every decode-time problem in the document against
// the schema type of target (a pointer to the decode destination).
//
// The document is parsed leniently into map[string]any (which can fail only
// on malformed TOML) and walked against the schema type (see walk.go): the
// walk finds every unknown field with an index-aware path and every shape
// mismatch, and is the single source of validation-style errors, because
// the real decoder abandons at its first problem and would lose the rest.
// The real decode stays strict — it populates target — and its failure
// is appended only when the walk has not already reported the same
// problem, so nothing is duplicated.
func decodeErrors(data []byte, target any) []ConfigError {
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		var decErr *toml.DecodeError
		if errors.As(err, &decErr) {
			return []ConfigError{{Field: keyPath(decErr.Key()), Msg: decErr.Error(), Err: err}}
		}
		return []ConfigError{{Msg: "decode: " + err.Error(), Err: err}}
	}

	var errs []ConfigError
	walkValue("", doc, reflect.TypeOf(target).Elem(), 0, &errs)

	strict := toml.NewDecoder(bytes.NewReader(data))
	strict.DisallowUnknownFields()
	if err := strict.Decode(target); err != nil {
		errs = appendUncoveredDecodeError(err, errs)
	}
	return errs
}

// appendUncoveredDecodeError adds the strict decoder's failure to errs only
// when the walk has not already reported the same problem. Unknown-field
// findings are always the walk's (its paths carry element indexes the
// decoder's do not); a type error is dropped only when a walk error already
// covers the same subtree.
func appendUncoveredDecodeError(err error, errs []ConfigError) []ConfigError {
	var missing *toml.StrictMissingError
	if errors.As(err, &missing) {
		for i := range missing.Errors {
			if walkSawUnknownField(errs) {
				continue
			}
			errs = append(errs, ConfigError{
				Field: keyPath(missing.Errors[i].Key()),
				Msg:   msgUnknownField,
			})
		}
		return errs
	}
	var decErr *toml.DecodeError
	if errors.As(err, &decErr) {
		if coveredByWalk(keyPath(decErr.Key()), errs) {
			return errs
		}
		return append(errs, ConfigError{Field: keyPath(decErr.Key()), Msg: decErr.Error(), Err: err})
	}
	return append(errs, ConfigError{Msg: "decode: " + err.Error(), Err: err})
}

// walkSawUnknownField reports whether any walk error is an unknown-field
// finding.
func walkSawUnknownField(errs []ConfigError) bool {
	for _, ce := range errs {
		if ce.Msg == msgUnknownField {
			return true
		}
	}
	return false
}

// coveredByWalk reports whether a decoder key path is already explained by
// a walk error: the walk path (with index segments stripped and map keys
// unquoted) equals or extends the decoder path, or the decoder path extends
// a walk path. A decoder error carrying no key path at all — go-toml loses
// the path when the failure happens through a map value — counts as
// covered whenever the walk reported anything: the walk's located reports
// are strictly more useful than a location-less duplicate, and an empty
// walk list keeps the decoder error (fail-closed).
func coveredByWalk(decoderPath string, errs []ConfigError) bool {
	if len(errs) == 0 {
		return false
	}
	if decoderPath == "" {
		return true
	}
	for _, ce := range errs {
		wp := unquoteKeySegments(stripIndexSegments(ce.Field))
		if wp == decoderPath ||
			strings.HasPrefix(wp+".", decoderPath+".") ||
			strings.HasPrefix(decoderPath+".", wp+".") {
			return true
		}
	}
	return false
}

// unquoteKeySegments removes TOML quoting from the map-key segments of a
// walk field path (labels."a.b".name becomes labels.a.b.name) so it
// compares equal with decoder key paths, which carry map keys bare.
// Malformed quoting is passed through unchanged.
func unquoteKeySegments(path string) string {
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		if path[i] != '"' {
			b.WriteByte(path[i])
			continue
		}
		end := quotedEnd(path, i)
		if end < 0 {
			b.WriteByte(path[i]) // unterminated quote; keep verbatim
			continue
		}
		if raw, err := strconv.Unquote(path[i : end+1]); err == nil {
			b.WriteString(raw)
		} else {
			b.WriteString(path[i+1 : end])
		}
		i = end
	}
	return b.String()
}

// quotedEnd returns the index of the closing double quote of the basic
// string starting at start, or -1 when the string is unterminated.
func quotedEnd(s string, start int) int {
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// keyPath renders a decoder key as a dotted field path.
func keyPath(key toml.Key) string {
	return strings.Join(key, ".")
}

// expandConfig walks every string field of cfg and expands ${VAR}
// references using lookup. Errors are anchored to the walked field path.
func expandConfig(cfg *Config, lookup Lookup) []ConfigError {
	var errs []ConfigError
	expandValue(reflect.ValueOf(cfg).Elem(), "", lookup, &errs)
	return errs
}

// expandValue expands strings in v in place. struct fields are visited in
// declaration order using their toml tag names; slice and array elements
// are addressed as field[i]; maps are rebuilt with every value expanded
// (values inside a map are not addressable in place), and each value's
// path carries the TOML-quoted map key (accounts."prod".rpm). Map keys
// themselves are never expanded. Fields tagged toml:"-" are loader-owned
// and skipped. Plain scalars (numbers, bools) cannot carry a reference and
// are skipped. Kinds that could hide a string the walk cannot reach
// (interfaces, channels, funcs, complex numbers) are reported as errors
// rather than silently skipped.
func expandValue(v reflect.Value, field string, lookup Lookup, errs *[]ConfigError) {
	switch v.Kind() {
	case reflect.String:
		expanded, err := Expand(v.String(), lookup)
		if err != nil {
			*errs = append(*errs, ConfigError{Field: field, Msg: err.Error()})
			return
		}
		if !v.CanSet() {
			*errs = append(*errs, ConfigError{Field: field, Msg: "cannot expand unsettable field"})
			return
		}
		v.SetString(expanded)
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue // unexported fields are unsettable and never TOML targets
			}
			if f.Tag.Get("toml") == "-" {
				continue // loader-owned fields (Warnings, Resolved) are not document data
			}
			expandValue(v.Field(i), joinField(field, tomlName(f)), lookup, errs)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			expandValue(v.Index(i), fmt.Sprintf("%s[%d]", field, i), lookup, errs)
		}
	case reflect.Pointer:
		if !v.IsNil() {
			expandValue(v.Elem(), field, lookup, errs)
		}
	case reflect.Map:
		expandMap(v, field, lookup, errs)
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		// Scalars cannot carry a ${...} reference; nothing to expand.
	default:
		*errs = append(*errs, ConfigError{
			Field: field,
			Msg:   fmt.Sprintf("unsupported field kind %s", v.Kind()),
		})
	}
}

// expandMap rebuilds v with every value expanded in place of the original.
// A nil map has nothing to expand and stays nil. Each value's path carries
// the TOML-quoted map key so expansion errors inside map-keyed tables are
// anchored exactly (accounts."prod".rpm), mirroring the walk's display
// style.
func expandMap(v reflect.Value, field string, lookup Lookup, errs *[]ConfigError) {
	if v.IsNil() {
		return
	}
	if !v.CanSet() {
		*errs = append(*errs, ConfigError{Field: field, Msg: "cannot expand unsettable field"})
		return
	}
	expanded := reflect.MakeMapWithSize(v.Type(), v.Len())
	iter := v.MapRange()
	for iter.Next() {
		val := reflect.New(v.Type().Elem()).Elem()
		val.Set(iter.Value())
		expandValue(val, joinField(field, quoteTomlKey(iter.Key().String())), lookup, errs)
		expanded.SetMapIndex(iter.Key(), val)
	}
	v.Set(expanded)
}

// joinField extends a parent path with a child segment.
func joinField(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

// tomlName returns the configuration key a struct field is decoded from.
func tomlName(f reflect.StructField) string {
	if tag, ok := f.Tag.Lookup("toml"); ok && tag != "" {
		if name, _, _ := strings.Cut(tag, ","); name != "" {
			return name
		}
	}
	return f.Name
}
