// Package proxy provides the pool-scoped HTTP/HTTPS and SOCKS proxy
// component used by Discobox sandboxes.
package proxy

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/proxy/gateway"
)

// Config controls a pool proxy instance.
type Config struct {
	ListenAddress string
	PublicURL     string
	CertDir       string
	DatabaseDSN   string
	Control       ControlConfig
	Cache         CacheConfig
	Recording     RecordingConfig
	Allowlist     AllowlistConfig
	Gateways      GatewaysConfig
	Secrets       SecretsConfig
	// Origins is the host the pool serves its sandboxes' Git origins at,
	// which the proxy answers by forwarding to the pool rather than sending to
	// the internet. Fixed for the life of the proxy: ApplyConfig leaves it.
	Origins OriginsConfig
	// Trusts are the host pins in force, each one client's (ADR 0149). They
	// are runtime policy: ApplyConfig replaces them whole.
	Trusts []HostTrust

	// UpstreamProxy forwards this proxy's own egress through another proxy.
	// Empty means direct, and falls back to the standard proxy environment
	// variables (see UpstreamProxyEnvVars). It exists for the nested case: a
	// pool proxy running inside a Discobox sandbox has no route off-box, so it
	// must hand traffic to the sandbox's own forwarder instead of dialing
	// origins itself.
	UpstreamProxy string

	// UpstreamNoProxy exempts destinations from UpstreamProxy, using the same
	// syntax as NO_PROXY. Empty falls back to the NO_PROXY environment
	// variable. Exemptions matter even when an upstream exists: the proxy
	// reaches its own control plane and loopback services directly.
	UpstreamNoProxy string
}

// SecretsConfig controls runtime sentinel secret swapping. The real values are
// resolved on demand through an injected resolver; this config carries only the
// non-secret sentinel strings and swap tuning.
type SecretsConfig struct {
	ScanQuery bool
	// PositiveTTLSeconds is the ceiling on how long a resolved value is held,
	// regardless of what the resolver says. A resolver may return an earlier
	// expiry and always wins; this bounds the case where it returns none at all
	// — a grant that never expires — so a resolved credential is never cached
	// indefinitely by a process that is not the one authorizing it.
	PositiveTTLSeconds     int64
	NegativeTTLSeconds     int64
	RefreshIntervalSeconds int64
	// ResolveRetrySeconds is how long a sentinel that failed to resolve for
	// any reason but a denial is asked about again before the request is
	// refused with a 503, rather than sent with the sentinel in it (ADR
	// 26-10-14-482 §2). Zero is five seconds, which outlasts a control-plane
	// restart.
	ResolveRetrySeconds int64
	Clients             []SecretClient
	// GateHost is a host the proxy never sends to the internet. A request for
	// it is answered by the resolver's Gate: admitted to the upstream behind it
	// only while it carries a live use of the credential that opens it, and
	// refused by the proxy otherwise (ADR 0140 §2). Empty is no gate.
	GateHost string
}

// DefaultSecretPositiveTTLSeconds bounds the positive resolution cache when no
// operator value is set. Five minutes is long enough that a busy sandbox is not
// re-resolving on every request, and short enough that a revocation the
// resolver would report takes effect on a human timescale.
const DefaultSecretPositiveTTLSeconds = 300

// SecretClient binds a client (sandbox) ID to the sentinel strings whose values
// the proxy swaps in that client's requests.
type SecretClient struct {
	ClientID  string
	Sentinels []string
}

// ControlConfig controls the optional read-only control API, which
// ListenAndServeControl serves when ListenAddress is set. The pool agent sets
// no ListenAddress and does not call it, so a pool's proxy serves no control
// API.
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
//
// Both queues are bounded and drop rather than block, so their depth is the
// burst the proxy can absorb before the audit log develops holes. QueueSize
// buffers small metadata rows shared by the whole proxy and is sized
// generously. StreamQueueSize is per upgraded stream and buffers raw payload
// chunks, so its depth multiplies by both chunk size and concurrent streams.
type RecordingConfig struct {
	Enabled         bool
	QueueSize       int
	MaxHeaderBytes  int
	StreamDir       string
	StreamQueueSize int
	BodyDir         string
	// Retention is how long an audit row and the spool files it names are kept.
	// Zero keeps them forever, which is what an embedder that manages the
	// database itself wants; every Discobox pool sets a window, because it is
	// the only thing that removes rows. Sandbox deletion deliberately does
	// not: the audit trail of a sandbox that has been deleted is the case it
	// exists for.
	Retention time.Duration
	// MaxSpoolBytes and MaxSpoolPercent bound the body and stream spool trees
	// together, by bytes rather than age (ADR 26-10-08-698): the budget is the
	// lesser of an absolute ceiling and a percentage of the filesystem holding
	// them. A zero term drops out, and with both zero the trees are bounded by
	// Retention alone.
	MaxSpoolBytes   int64
	MaxSpoolPercent float64
	// BodyHeadBytes is what a spool file keeps when the budget truncates it.
	BodyHeadBytes int64
}

// AllowlistConfig is the destination policy (ADR 26-10-01-032 §6): each
// restricted client's allow list, keyed by client ID (the sandbox ID). A client
// absent from Clients is unrestricted; one present reaches only its entries,
// so an empty list reaches nothing. Entries are egress.ValidateEntry's.
type AllowlistConfig struct {
	Clients map[string][]string
}

// GatewaysConfig is each client's gateways (ADR 26-10-16-470), keyed by client
// ID (the sandbox ID): the routes its requests are forwarded by once every
// other policy has judged them as the sandbox sent them. A client absent from
// Clients has none. They are validated where they are written, never here
// (§1): one route this proxy reads differently is skipped
// (gateway.NewTable), where refusing it would stop every other client's policy
// from being applied with it.
type GatewaysConfig struct {
	Clients map[string][]gateway.Route
}

const (
	// DefaultRetention is how long audit rows, recorded bodies, and upgraded
	// stream captures are kept. Two days spans a weekend day plus the working
	// day either side of it, which is the window in which someone actually goes
	// looking at what a sandbox sent.
	DefaultRetention = 48 * time.Hour

	// DefaultMaxSpoolBytes and DefaultMaxSpoolPercent are the spool budget's
	// terms: 5% of the default 100 GiB pool VM data disk is 5 GiB, and the
	// ceiling applies once a Docker pool's host filesystem reaches 2 TB.
	DefaultMaxSpoolBytes   = 100 << 30
	DefaultMaxSpoolPercent = 5
	// DefaultBodyHeadBytes keeps enough of a body to identify it: a status
	// line, a JSON envelope, an archive's first entries.
	DefaultBodyHeadBytes = 64 << 10

	// minSweepInterval and maxSweepInterval bound how often a retention pass
	// runs, whatever the window is set to.
	minSweepInterval = time.Minute
	maxSweepInterval = time.Hour
)

// SweepInterval is how often to run a retention pass for a given window.
//
// It is derived rather than configured for the same reason imagereap derives
// its own: the two are useless apart. Half the window bounds how far past the
// retention a row can survive at 1.5x, and the clamps stop a very short window
// becoming a busy loop or a very long one leaving the first pass days away.
func SweepInterval(retention time.Duration) time.Duration {
	interval := retention / 2
	if interval < minSweepInterval {
		return minSweepInterval
	}
	if interval > maxSweepInterval {
		return maxSweepInterval
	}
	return interval
}

// DefaultConfig returns conservative pool proxy defaults.
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
			QueueSize:       16384,
			MaxHeaderBytes:  64 * 1024,
			StreamDir:       "./proxy-streams",
			StreamQueueSize: 1024,
			BodyDir:         "./proxy-bodies",
			Retention:       DefaultRetention,
			MaxSpoolBytes:   DefaultMaxSpoolBytes,
			MaxSpoolPercent: DefaultMaxSpoolPercent,
			BodyHeadBytes:   DefaultBodyHeadBytes,
		},
		Secrets: SecretsConfig{
			PositiveTTLSeconds: DefaultSecretPositiveTTLSeconds,
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
		if c.Recording.Retention < 0 {
			return errors.New("recording retention cannot be negative")
		}
		if c.Recording.MaxSpoolBytes < 0 {
			return errors.New("recording max spool bytes cannot be negative")
		}
		if c.Recording.MaxSpoolPercent < 0 || c.Recording.MaxSpoolPercent > 100 {
			return errors.New("recording max spool percent must be within [0, 100]")
		}
		if c.Recording.BodyHeadBytes < 0 {
			return errors.New("recording body head bytes cannot be negative")
		}
		if (c.Recording.MaxSpoolBytes > 0 || c.Recording.MaxSpoolPercent > 0) && c.Recording.BodyHeadBytes == 0 {
			return errors.New("recording body head bytes must be positive when a spool budget is set")
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
	for _, trust := range c.Trusts {
		if err := trust.Validate(); err != nil {
			return err
		}
	}
	for clientID, allow := range c.Allowlist.Clients {
		if strings.TrimSpace(clientID) == "" {
			return errors.New("allowlist has an empty client ID")
		}
		if err := (egress.Policy{Allow: allow}).Validate(); err != nil {
			return fmt.Errorf("allowlist for client %q: %w", clientID, err)
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
