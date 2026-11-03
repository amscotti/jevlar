package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Validate checks every rule the boot path enforces and reports all
// violations at once, each anchored to a field path: it returns nil when the
// configuration is valid, otherwise an error joining one ConfigError per
// violation (see ValidationErrors; errors.As extracts the parts). Callers
// must not bind a listener when it returns a non-nil error.
//
// The rule set is the v1 configuration contract. Every rule reports its own
// field path; map-keyed tables address entries with TOML-quoted keys
// (models."jev-latest".policy).
func (c *Config) Validate() error {
	var errs []ConfigError
	errs = append(errs, c.validateServer()...)
	errs = append(errs, c.validateUsage()...)
	errs = append(errs, c.validateAuth()...)
	errs = append(errs, c.validateAccounts()...)
	errs = append(errs, c.validateProviders()...)
	errs = append(errs, c.validateModels()...)
	errs = append(errs, c.validateBreaker()...)
	return joined(errs)
}

// validateServer checks the listener, admin, timeout, caps and logging
// rules.
func (c *Config) validateServer() []ConfigError {
	var errs []ConfigError
	if c.ConfigVersion != 1 {
		errs = append(errs, ConfigError{
			Field: "config_version",
			Msg:   fmt.Sprintf("unsupported version %d (supported: 1)", c.ConfigVersion),
		})
	}
	errs = append(errs, validateListen("server.listen", c.Server.Listen)...)
	errs = append(errs, c.validateAdminListen()...)

	switch {
	case c.Server.LogLevel == "":
		// Empty means the default, which is valid.
	case isValidLogLevel(c.Server.LogLevel):
	default:
		errs = append(errs, ConfigError{
			Field: "server.log_level",
			Msg:   `must be one of "debug", "info", "warn", "error"`,
		})
	}
	if c.Server.LogBodies && c.Server.LogLevel != "debug" {
		errs = append(errs, ConfigError{
			Field: "server.log_bodies",
			Msg:   `requires log_level = "debug"`,
		})
	}
	if c.Server.RequestTimeoutMs < 0 {
		errs = append(errs, ConfigError{
			Field: "server.request_timeout_ms",
			Msg:   "must be at least 1",
		})
	}
	if c.Server.ShutdownGraceMs < 0 || c.Server.ShutdownGraceMs > MaxShutdownGraceMs {
		errs = append(errs, ConfigError{
			Field: "server.shutdown_grace_ms",
			Msg:   "must be between 0 and 600000",
		})
	}
	if c.Server.ReadHeaderTimeoutMs < 0 {
		errs = append(errs, ConfigError{
			Field: "server.read_header_timeout_ms",
			Msg:   "must be at least 1",
		})
	}
	if c.Server.MaxBodyBytes != 0 && c.Server.MaxBodyBytes < 1024 {
		errs = append(errs, ConfigError{
			Field: "server.max_body_bytes",
			Msg:   "must be at least 1024",
		})
	}
	if c.Server.MaxResponseBytes != 0 && c.Server.MaxResponseBytes < 1024 {
		errs = append(errs, ConfigError{
			Field: "server.max_response_bytes",
			Msg:   "must be at least 1024",
		})
	}
	if c.Server.MaxInflight < 0 {
		errs = append(errs, ConfigError{
			Field: "server.max_inflight",
			Msg:   "must be at least 1",
		})
	}
	if c.Server.AdminToken != "" && len(c.Server.AdminToken) < 32 {
		errs = append(errs, ConfigError{
			Field: "server.admin_token",
			Msg:   "must be at least 32 characters",
		})
	}
	return errs
}

// validateAdminListen checks the admin listener rules: a set admin_listen
// must parse as host:port, must be loopback when no admin token protects
// it, and must differ from the main listener. An empty admin_listen
// disables the admin plane and is valid.
func (c *Config) validateAdminListen() []ConfigError {
	if c.Server.AdminListen == "" {
		return nil
	}
	var errs []ConfigError
	errs = append(errs, validateListen("server.admin_listen", c.Server.AdminListen)...)
	host, _, err := net.SplitHostPort(c.Server.AdminListen)
	if err != nil {
		return errs // already reported by the parse check
	}
	if c.Server.AdminToken == "" && !isLoopbackHost(host) {
		errs = append(errs, ConfigError{
			Field: "server.admin_listen",
			Msg:   "must be loopback when admin_token is empty",
		})
	}
	if listenersConflict(c.Server.Listen, c.Server.AdminListen) {
		errs = append(errs, ConfigError{
			Field: "server.admin_listen",
			Msg:   "must differ from server.listen (same port on an overlapping address)",
		})
	}
	return errs
}

// listenersConflict reports whether two host:port addresses would contend
// for the same socket. It catches what net.Listen would fail closed on
// later: identical strings, wildcard binds (empty host, 0.0.0.0, ::)
// against any address in the same family, and localhost against a loopback
// IP. It is deliberately conservative about distinct hostnames: resolution
// is not this package's job.
func listenersConflict(a, b string) bool {
	aHost, aPort, errA := net.SplitHostPort(a)
	bHost, bPort, errB := net.SplitHostPort(b)
	if errA != nil || errB != nil || aPort != bPort {
		return false
	}
	if aHost == "" || bHost == "" || strings.EqualFold(aHost, bHost) {
		return true
	}
	aIP := resolveLocalHost(aHost)
	bIP := resolveLocalHost(bHost)
	if aIP == nil || bIP == nil {
		return false
	}
	if aIP.IsUnspecified() {
		return bIP.IsUnspecified() || (aIP.To4() != nil) == (bIP.To4() != nil)
	}
	if bIP.IsUnspecified() {
		return (aIP.To4() != nil) == (bIP.To4() != nil)
	}
	return aIP.Equal(bIP)
}

// resolveLocalHost parses an IP literal or the name "localhost" into an IP;
// other names return nil.
func resolveLocalHost(host string) net.IP {
	if ip := net.ParseIP(host); ip != nil {
		return ip
	}
	if strings.EqualFold(host, "localhost") {
		return net.IPv4(127, 0, 0, 1)
	}
	return nil
}

// validateUsage checks the rollup rules: when rollups are enabled the
// directory must exist and be writable, and retention and size bounds must
// be set (explicitly or by default).
func (c *Config) validateUsage() []ConfigError {
	if !c.Usage.Enabled {
		return nil
	}
	var errs []ConfigError
	if err := probeWritableDir(c.Usage.Dir); err != nil {
		errs = append(errs, ConfigError{
			Field: "usage.dir",
			Msg:   fmt.Sprintf("%q is not writable", c.Usage.Dir),
		})
	}
	if c.Usage.RetentionDays < 1 {
		errs = append(errs, ConfigError{
			Field: "usage.retention_days",
			Msg:   "must be at least 1",
		})
	}
	if c.Usage.MaxFileMB < 1 {
		errs = append(errs, ConfigError{
			Field: "usage.max_file_mb",
			Msg:   "must be at least 1",
		})
	}
	return errs
}

// probeWritableDir reports whether dir exists and a file can be created in
// it: the probe is created and removed immediately.
func probeWritableDir(dir string) error {
	probe, err := os.CreateTemp(dir, ".jevlar-probe-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		return err
	}
	return os.Remove(name)
}

// validateAuth checks the inbound client keys: unique non-empty ids, hashes
// that are exactly 64 hex characters, one id per hash, and non-negative
// quotas.
func (c *Config) validateAuth() []ConfigError {
	var errs []ConfigError
	ids := make(map[string]bool, len(c.Auth.Keys))
	hashes := make(map[string]string, len(c.Auth.Keys))
	for i, k := range c.Auth.Keys {
		if k.ID == "" {
			errs = append(errs, ConfigError{
				Field: fmt.Sprintf("auth.keys[%d].id", i),
				Msg:   "id is required",
			})
		} else if ids[k.ID] {
			errs = append(errs, ConfigError{
				Field: fmt.Sprintf("auth.keys[%d].id", i),
				Msg:   fmt.Sprintf("duplicate %q", k.ID),
			})
		} else {
			ids[k.ID] = true
			if !validIdentifier(k.ID) {
				errs = append(errs, ConfigError{
					Field: fmt.Sprintf("auth.keys[%d].id", i),
					Msg:   "id must match [A-Za-z0-9._:-]+ and be at most 200 characters",
				})
			}
		}
		if !isHex64(k.Hash) {
			errs = append(errs, ConfigError{
				Field: fmt.Sprintf("auth.keys[%d].hash", i),
				Msg:   "must be 64 hex characters",
			})
		} else if first, dup := hashes[k.Hash]; dup {
			errs = append(errs, ConfigError{
				Field: fmt.Sprintf("auth.keys[%d].hash", i),
				Msg:   fmt.Sprintf("hash already used by key %q", first),
			})
		} else {
			hashes[k.Hash] = k.ID
		}
		if k.RPM < 0 {
			errs = append(errs, ConfigError{
				Field: fmt.Sprintf("auth.keys[%d].rpm", i),
				Msg:   "must be >= 0",
			})
		}
		if k.TPM < 0 {
			errs = append(errs, ConfigError{
				Field: fmt.Sprintf("auth.keys[%d].tpm", i),
				Msg:   "must be >= 0",
			})
		}
	}
	return errs
}

// isHex64 reports whether s is exactly 64 hexadecimal characters
// (the SHA-256 hex form `jevlar keyhash` prints).
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] >= '0' && s[i] <= '9',
			s[i] >= 'a' && s[i] <= 'f',
			s[i] >= 'A' && s[i] <= 'F':
		default:
			return false
		}
	}
	return true
}

// validateAccounts checks the account quotas in sorted name order so the
// error list is deterministic.
func (c *Config) validateAccounts() []ConfigError {
	names := make([]string, 0, len(c.Accounts))
	for name := range c.Accounts {
		names = append(names, name)
	}
	sort.Strings(names)
	var errs []ConfigError
	for _, name := range names {
		a := c.Accounts[name]
		field := "accounts." + strconv.Quote(name)
		if !validIdentifier(name) {
			errs = append(errs, ConfigError{
				Field: field,
				Msg:   "account name must match [A-Za-z0-9._:-]+ and be at most 200 characters",
			})
		}
		if a.RPM < 0 {
			errs = append(errs, ConfigError{
				Field: field + ".rpm",
				Msg:   "must be >= 0",
			})
		}
		if a.TPM < 0 {
			errs = append(errs, ConfigError{
				Field: field + ".tpm",
				Msg:   "must be >= 0",
			})
		}
	}
	return errs
}

// validateProviders checks the provider rules: presence, unique names, base
// URL shape and scheme policy, path shape, auth/key consistency, model
// lists, timeout and concurrency bounds, the request-deadline ceiling, and
// account references.
func (c *Config) validateProviders() []ConfigError {
	var errs []ConfigError
	if len(c.Providers) == 0 {
		return []ConfigError{{
			Field: "providers",
			Msg:   "at least one provider is required",
		}}
	}
	seen := make(map[string]bool, len(c.Providers))
	for i, p := range c.Providers {
		field := fmt.Sprintf("providers[%d]", i)
		if p.Name == "" {
			errs = append(errs, ConfigError{
				Field: field + ".name",
				Msg:   "name is required",
			})
		} else if seen[p.Name] {
			errs = append(errs, ConfigError{
				Field: field + ".name",
				Msg:   fmt.Sprintf("duplicate %q", p.Name),
			})
		} else {
			seen[p.Name] = true
			if !validIdentifier(p.Name) {
				errs = append(errs, ConfigError{
					Field: field + ".name",
					Msg:   "name must match [A-Za-z0-9._:-]+ and be at most 200 characters",
				})
			}
		}
		errs = append(errs, validateBaseURL(field+".base_url", p.BaseURL)...)
		errs = append(errs, validatePath(field+".path", p.Path)...)
		errs = append(errs, validateProviderAuth(field, &c.Providers[i])...)
		errs = append(errs, validateProviderModels(field+".models", p.Models)...)
		if p.TimeoutMs < 1 || p.TimeoutMs > MaxProviderTimeoutMs {
			errs = append(errs, ConfigError{
				Field: field + ".timeout_ms",
				Msg:   fmt.Sprintf("must be between 1 and %d", MaxProviderTimeoutMs),
			})
		} else if p.TimeoutMs > c.Server.RequestTimeoutMs {
			errs = append(errs, ConfigError{
				Field: field + ".timeout_ms",
				Msg:   "must not exceed server.request_timeout_ms",
			})
		}
		if p.MaxConcurrency < 1 {
			errs = append(errs, ConfigError{
				Field: field + ".max_concurrency",
				Msg:   "must be at least 1",
			})
		}
		if p.Account != "" {
			if _, ok := c.Accounts[p.Account]; !ok {
				errs = append(errs, ConfigError{
					Field: field + ".account",
					Msg:   fmt.Sprintf("unknown account %q", p.Account),
				})
			}
		}
	}
	return errs
}

// validateBaseURL checks the origin rules: the value must parse as a URL,
// carry an http or https scheme, keep plain http for loopback hosts only,
// and carry no path, query or fragment (the request path belongs in
// providers[].path).
func validateBaseURL(field, raw string) []ConfigError {
	if raw == "" {
		return []ConfigError{{Field: field, Msg: "is required"}}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return []ConfigError{{Field: field, Msg: fmt.Sprintf("must be a URL (%v)", err)}}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return []ConfigError{{Field: field, Msg: fmt.Sprintf("unsupported scheme %q (use http or https)", u.Scheme)}}
	}
	if u.Host == "" {
		return []ConfigError{{Field: field, Msg: "must be scheme://host[:port] with no path (use providers[].path)"}}
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return []ConfigError{{Field: field, Msg: fmt.Sprintf("scheme %q is only allowed for loopback", u.Scheme)}}
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.RawFragment != "" || u.User != nil {
		return []ConfigError{{Field: field, Msg: "must be scheme://host[:port] with no path (use providers[].path)"}}
	}
	return nil
}

// isLoopbackHost reports whether host (a bare hostname or IP literal, no
// port) names this machine: an IP literal must be loopback, and the name
// "localhost" is accepted case-insensitively.
func isLoopbackHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

// validatePath checks the upstream request path rules.
func validatePath(field, path string) []ConfigError {
	if !strings.HasPrefix(path, "/") {
		return []ConfigError{{Field: field, Msg: `must start with "/"`}}
	}
	if strings.Contains(path, "..") {
		return []ConfigError{{Field: field, Msg: `must not contain ".."`}}
	}
	return nil
}

// validateProviderAuth checks auth-mode/key consistency: the mode must be
// known, bearer auth requires at least one key, and auth "none" must define
// none. An empty mode means the default ("none").
func validateProviderAuth(field string, p *Provider) []ConfigError {
	var errs []ConfigError
	auth := p.Auth
	if auth == "" {
		auth = AuthNone
	}
	switch auth {
	case AuthNone, AuthBearer:
	default:
		errs = append(errs, ConfigError{
			Field: field + ".auth",
			Msg:   fmt.Sprintf("must be %q or %q", AuthNone, AuthBearer),
		})
	}
	switch auth {
	case AuthBearer:
		if len(p.APIKeys) == 0 {
			errs = append(errs, ConfigError{
				Field: field + ".api_keys",
				Msg:   "bearer auth requires at least one key",
			})
		}
	case AuthNone:
		if len(p.APIKeys) > 0 {
			errs = append(errs, ConfigError{
				Field: field + ".api_keys",
				Msg:   fmt.Sprintf("auth %q must not define keys", AuthNone),
			})
		}
	}
	seenKeys := make(map[string]bool, len(p.APIKeys))
	for j, k := range p.APIKeys {
		keyField := fmt.Sprintf("%s.api_keys[%d]", field, j)
		switch {
		case k.ID == "":
			errs = append(errs, ConfigError{Field: keyField + ".id", Msg: "id is required"})
		case seenKeys[k.ID]:
			errs = append(errs, ConfigError{
				Field: keyField + ".id",
				Msg:   fmt.Sprintf("duplicate %q", k.ID),
			})
		default:
			seenKeys[k.ID] = true
			if !validIdentifier(k.ID) {
				errs = append(errs, ConfigError{
					Field: keyField + ".id",
					Msg:   "id must match [A-Za-z0-9._:-]+ and be at most 200 characters",
				})
			}
		}
	}
	return errs
}

// validateProviderModels checks the provider's model surface: entries must
// be non-empty and unique. An empty list means the surface is unspecified
// and is accepted.
func validateProviderModels(field string, models []string) []ConfigError {
	seen := make(map[string]bool, len(models))
	var errs []ConfigError
	for _, m := range models {
		if m == "" {
			errs = append(errs, ConfigError{Field: field, Msg: "model names must not be empty"})
			continue
		}
		if !validIdentifier(m) {
			errs = append(errs, ConfigError{
				Field: field,
				Msg:   fmt.Sprintf("model id %q must match [A-Za-z0-9._:-]+ and be at most 200 characters", m),
			})
			continue
		}
		if seen[m] {
			errs = append(errs, ConfigError{Field: field, Msg: fmt.Sprintf("duplicate %q", m)})
			continue
		}
		seen[m] = true
	}
	return errs
}

// validIdentifier reports whether s is a safe identity for logs and metric
// labels. It shares the model-id charset so an identifier can never carry a
// control character into a log line or an arbitrary string into a metric
// label.
func validIdentifier(s string) bool { return validModelID(s) }

// validModelID reports whether id is a safe routing identifier. The
// charset is deliberately narrow: the model value is spliced into a JSON
// body byte-for-byte, so an id containing a quote, a backslash or any
// control character must never reach the request path.
func validModelID(id string) bool {
	if id == "" || len(id) > 200 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '.', c == '_', c == ':', c == '-':
		default:
			return false
		}
	}
	return true
}

// validateModels checks every model alias in sorted order: targets must
// name an existing provider and a non-empty model that the provider offers
// when it lists models, the policy must be known, and the fastest policy's
// tuning knobs must be in range.
func (c *Config) validateModels() []ConfigError {
	aliases := make([]string, 0, len(c.Models))
	for alias := range c.Models {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)

	providers := make(map[string]*Provider, len(c.Providers))
	for i := range c.Providers {
		providers[c.Providers[i].Name] = &c.Providers[i]
	}

	var errs []ConfigError
	for _, alias := range aliases {
		m := c.Models[alias]
		field := "models." + strconv.Quote(alias)
		if !validModelID(alias) {
			errs = append(errs, ConfigError{
				Field: field,
				Msg:   "model alias must match [A-Za-z0-9._:-]+ and be at most 200 characters",
			})
		}
		if m.ReleaseDate != "" {
			if _, err := time.Parse("2006-01-02", m.ReleaseDate); err != nil {
				errs = append(errs, ConfigError{
					Field: field + ".release_date",
					Msg:   "must be a YYYY-MM-DD date",
				})
			}
		}
		if len(m.Description) > 500 {
			errs = append(errs, ConfigError{
				Field: field + ".description",
				Msg:   "must be at most 500 characters",
			})
		}
		if len(m.Targets) == 0 {
			errs = append(errs, ConfigError{Field: field + ".targets", Msg: "at least one target is required"})
		}
		for i, t := range m.Targets {
			targetField := fmt.Sprintf("%s.targets[%d]", field, i)
			if t.Provider == "" {
				errs = append(errs, ConfigError{Field: targetField + ".provider", Msg: "provider is required"})
				continue
			}
			p, ok := providers[t.Provider]
			if !ok {
				errs = append(errs, ConfigError{
					Field: targetField + ".provider",
					Msg:   fmt.Sprintf("unknown provider %q", t.Provider),
				})
				continue
			}
			if t.Model == "" {
				errs = append(errs, ConfigError{Field: targetField + ".model", Msg: "model must not be empty"})
				continue
			}
			if !validModelID(t.Model) {
				errs = append(errs, ConfigError{
					Field: targetField + ".model",
					Msg:   "model id must match [A-Za-z0-9._:-]+ and be at most 200 characters",
				})
				continue
			}
			if len(p.Models) > 0 && !containsString(p.Models, t.Model) {
				errs = append(errs, ConfigError{
					Field: targetField + ".model",
					Msg:   fmt.Sprintf("%q is not offered by provider %q", t.Model, t.Provider),
				})
			}
		}
		switch m.Policy {
		case "":
			// Empty means the default ("priority"), applied at parse time.
		case PolicyPriority, PolicyFastest:
		default:
			errs = append(errs, ConfigError{
				Field: field + ".policy",
				Msg:   "must be priority or fastest",
			})
		}
		if m.MaxAttempts < 1 {
			errs = append(errs, ConfigError{Field: field + ".max_attempts", Msg: "must be at least 1"})
		}
		// The tuning knobs are validated regardless of the selected policy:
		// a negative value left behind by a policy switch is still garbage,
		// and defaults fill every field at parse time.
		if m.EwmaAlpha <= 0 || m.EwmaAlpha > 1 {
			errs = append(errs, ConfigError{Field: field + ".ewma_alpha", Msg: "must be in (0, 1]"})
		}
		if m.HysteresisMs < 0 {
			errs = append(errs, ConfigError{Field: field + ".hysteresis_ms", Msg: "must be at least 0"})
		}
		if m.ExploreEvery < 1 {
			errs = append(errs, ConfigError{Field: field + ".explore_every", Msg: "must be at least 1"})
		}
	}
	return errs
}

// containsString reports whether s is in list.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// validateBreaker checks the circuit-breaker bounds.
func (c *Config) validateBreaker() []ConfigError {
	var errs []ConfigError
	if c.Breaker.Threshold < 1 {
		errs = append(errs, ConfigError{Field: "breaker.threshold", Msg: "must be at least 1"})
	}
	if c.Breaker.CooldownMs < 100 {
		errs = append(errs, ConfigError{Field: "breaker.cooldown_ms", Msg: "must be at least 100"})
	}
	return errs
}

// isValidLogLevel reports whether s is a known log level.
func isValidLogLevel(s string) bool {
	switch s {
	case "debug", "info", "warn", "error":
		return true
	default:
		return false
	}
}

// validateListen reports whether listen is a usable host:port. The port must
// be strictly numeric: strconv would happily accept "+8080", so each byte is
// checked to be a digit first.
func validateListen(field, listen string) []ConfigError {
	_, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return []ConfigError{{Field: field, Msg: fmt.Sprintf("must be a host:port (%v)", err)}}
	}
	if !isDigits(portStr) {
		return []ConfigError{{Field: field, Msg: fmt.Sprintf("port %q is not numeric", portStr)}}
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return []ConfigError{{Field: field, Msg: fmt.Sprintf("port %q: %v", portStr, err)}}
	}
	if port < 1 || port > 65535 {
		return []ConfigError{{Field: field, Msg: fmt.Sprintf("port %d is outside 1..65535", port)}}
	}
	return nil
}

// isDigits reports whether s is one or more ASCII digits and nothing else.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
