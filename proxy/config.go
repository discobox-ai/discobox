// Package proxy provides the worker-scoped HTTP/HTTPS and SOCKS proxy
// component used by Discobox sandboxes.
package proxy

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"
)

// Config controls a worker proxy instance.
type Config struct {
	ListenAddress string
	PublicURL     string
	CertDir       string
	DatabaseDSN   string
	Control       ControlConfig
	Cache         CacheConfig
	Recording     RecordingConfig
	Allowlist     AllowlistConfig
	Headers       []HeaderRule
	Secrets       SecretsConfig
}

// SecretsConfig controls runtime sentinel secret swapping. The real values are
// resolved on demand through an injected resolver; this config carries only the
// non-secret sentinel strings and swap tuning.
type SecretsConfig struct {
	ScanQuery          bool
	PositiveTTLSeconds int64
	NegativeTTLSeconds int64
	Clients            []SecretClient
}

// SecretClient binds a client (sandbox) ID to the sentinel strings whose values
// the proxy swaps in that client's requests.
type SecretClient struct {
	ClientID  string
	Sentinels []string
}

// ControlConfig controls the optional read-only control API.
type ControlConfig struct {
	ListenAddress  string
	TrustPublicKey string
	ProjectID      string
	WorkerID       string
}

// CacheConfig controls disk-backed response caching.
type CacheConfig struct {
	Enabled      bool
	Dir          string
	MaxSizeBytes int64
	Patterns     []string
	ContentAware bool
}

// RecordingConfig controls audit persistence.
type RecordingConfig struct {
	Enabled         bool
	QueueSize       int
	MaxHeaderBytes  int
	StreamDir       string
	StreamQueueSize int
	BodyDir         string
}

// AllowlistConfig controls destination filtering.
type AllowlistConfig struct {
	Enabled bool
	Domains []string
	IPs     []string
	Rules   []AllowlistRule
}

// AllowlistRule scopes destination allowlist entries to authenticated clients.
type AllowlistRule struct {
	ClientIDs []string
	Domains   []string
	IPs       []string
}

// HeaderRule defines deterministic request header rewrites.
type HeaderRule struct {
	ID          string
	Pattern     string
	Methods     []string
	PathRegexes []string
	ClientIDs   []string
	Conditions  []HeaderCondition
	Set         map[string]string
	Append      map[string]string
}

// HeaderCondition requires an incoming request header value to match exactly.
type HeaderCondition struct {
	Header string
	Equals string
}

// DefaultConfig returns conservative worker proxy defaults.
func DefaultConfig() Config {
	return Config{
		ListenAddress: "127.0.0.1:17080",
		PublicURL:     "https://127.0.0.1:17080",
		CertDir:       "./proxy-certs",
		DatabaseDSN:   "./proxy-audit.db",
		Cache: CacheConfig{
			Dir:          "./proxy-cache",
			MaxSizeBytes: 20 * 1024 * 1024 * 1024,
		},
		Recording: RecordingConfig{
			Enabled:         true,
			QueueSize:       1024,
			MaxHeaderBytes:  64 * 1024,
			StreamDir:       "./proxy-streams",
			StreamQueueSize: 256,
			BodyDir:         "./proxy-bodies",
		},
	}
}

// Validate checks proxy configuration before runtime startup.
func (c Config) Validate() error {
	if strings.TrimSpace(c.ListenAddress) == "" {
		return errors.New("listen address is required")
	}
	if _, _, err := net.SplitHostPort(c.ListenAddress); err != nil {
		return fmt.Errorf("invalid listen address: %w", err)
	}
	if strings.TrimSpace(c.CertDir) == "" {
		return errors.New("cert dir is required")
	}
	if c.Recording.Enabled {
		if strings.TrimSpace(c.DatabaseDSN) == "" {
			return errors.New("database DSN is required when recording is enabled")
		}
		if c.Recording.QueueSize < 0 {
			return errors.New("recording queue size cannot be negative")
		}
		if c.Recording.StreamQueueSize < 0 {
			return errors.New("recording stream queue size cannot be negative")
		}
	}
	if strings.TrimSpace(c.Control.ListenAddress) != "" {
		if _, _, err := net.SplitHostPort(c.Control.ListenAddress); err != nil {
			return fmt.Errorf("invalid control listen address: %w", err)
		}
	}
	if c.Cache.Enabled {
		if strings.TrimSpace(c.Cache.Dir) == "" {
			return errors.New("cache dir is required when cache is enabled")
		}
		if c.Cache.MaxSizeBytes <= 0 {
			return errors.New("cache max size must be positive when cache is enabled")
		}
	}
	for _, rule := range c.Allowlist.Rules {
		if err := validateAllowlistRule(rule); err != nil {
			return err
		}
	}
	for _, rule := range c.Headers {
		if err := validateHeaderRule(rule); err != nil {
			return err
		}
	}
	for _, client := range c.Secrets.Clients {
		if err := validateSecretClient(client); err != nil {
			return err
		}
	}
	return nil
}

func validateSecretClient(client SecretClient) error {
	if strings.TrimSpace(client.ClientID) == "" {
		return errors.New("secret client ID is required")
	}
	if len(client.Sentinels) == 0 {
		return fmt.Errorf("secret client %q must include at least one sentinel", client.ClientID)
	}
	for _, sentinel := range client.Sentinels {
		if strings.TrimSpace(sentinel) == "" {
			return fmt.Errorf("secret client %q has empty sentinel", client.ClientID)
		}
	}
	return nil
}

func validateAllowlistRule(rule AllowlistRule) error {
	if len(rule.Domains) == 0 && len(rule.IPs) == 0 {
		return errors.New("allowlist rule must include at least one domain or IP")
	}
	for _, clientID := range rule.ClientIDs {
		if strings.TrimSpace(clientID) == "" {
			return errors.New("allowlist rule has empty client ID")
		}
	}
	return nil
}

func validateHeaderRule(rule HeaderRule) error {
	if strings.TrimSpace(rule.Pattern) == "" {
		return errors.New("header rule pattern is required")
	}
	if len(rule.Set) == 0 && len(rule.Append) == 0 {
		return fmt.Errorf("header rule %q must set or append at least one header", rule.ID)
	}
	for _, condition := range rule.Conditions {
		if strings.TrimSpace(condition.Header) == "" {
			return fmt.Errorf("header rule %q has condition with empty header", rule.ID)
		}
	}
	for _, method := range rule.Methods {
		if strings.TrimSpace(method) == "" {
			return fmt.Errorf("header rule %q has empty method", rule.ID)
		}
	}
	for _, pattern := range rule.PathRegexes {
		if strings.TrimSpace(pattern) == "" {
			return fmt.Errorf("header rule %q has empty path regex", rule.ID)
		}
		if _, err := regexp.Compile(pattern); err != nil {
			return fmt.Errorf("header rule %q has invalid path regex %q: %w", rule.ID, pattern, err)
		}
	}
	for _, clientID := range rule.ClientIDs {
		if strings.TrimSpace(clientID) == "" {
			return fmt.Errorf("header rule %q has empty client ID", rule.ID)
		}
	}
	for key := range rule.Set {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("header rule %q has empty set header name", rule.ID)
		}
	}
	for key := range rule.Append {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("header rule %q has empty append header name", rule.ID)
		}
	}
	return nil
}

// ClientMaterial describes the proxy configuration distributed to a sandbox.
type ClientMaterial struct {
	ClientID        string
	ProxyURL        string
	HTTPProxy       string
	HTTPSProxy      string
	AllProxy        string
	NoProxy         string
	MITMCAPath      string
	MTLSCAPath      string
	ClientCertPath  string
	ClientKeyPath   string
	GeneratedAt     time.Time
	ExpiresAt       time.Time
	EnvironmentVars map[string]string
}
