package proxyagent

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/dustin/go-humanize"

	"github.com/discobox-ai/discobox/proxy"
)

const (
	// EnvAuditMaxSize, EnvAuditMaxPercent and EnvAuditBodyHead override the
	// pool proxy's audit spool budget (ADR 26-10-08-698 §5): its absolute
	// ceiling, its share of the filesystem, and what a truncated file keeps.
	// They are set on the pool container by the server, from the backing
	// provider instance's configuration, as EnvAuditRetention is.
	EnvAuditMaxSize    = "DISCOBOX_PROXY_AUDIT_MAX_SIZE"
	EnvAuditMaxPercent = "DISCOBOX_PROXY_AUDIT_MAX_PERCENT"
	EnvAuditBodyHead   = "DISCOBOX_PROXY_AUDIT_BODY_HEAD"
)

// AuditSpoolBudget is the pool proxy's audit spool budget as a pool provider
// instance is configured with it. It is embedded into PoolPolicy and the Docker
// engine's Config, so the same fields flatten into both.
//
// Each value is kept as written and passed to the pool verbatim. An unset field
// serializes away, which leaves an existing pool's configuration and revision
// unchanged, while an explicit "0" stays distinct from unset: it drops that term
// from the budget.
type AuditSpoolBudget struct {
	// MaxSize is the budget's absolute ceiling ("100GiB").
	MaxSize ByteSize `json:"proxyAuditMaxSize,omitempty"`
	// MaxPercent is the budget's share of the filesystem holding the spool
	// ("5" or "5%").
	MaxPercent Percent `json:"proxyAuditMaxPercent,omitempty"`
	// BodyHead is what a file keeps when the budget truncates it ("64KiB").
	// Unlike the budget's terms it has no zero: a head of nothing is a
	// deletion, and the pool proxy refuses it at start.
	BodyHead HeadSize `json:"proxyAuditBodyHead,omitempty"`
}

// SetAuditSpoolEnv renders the configured fields into a pool container's environment.
func (b AuditSpoolBudget) SetAuditSpoolEnv(env map[string]string) {
	for name, value := range map[string]string{
		EnvAuditMaxSize:    string(b.MaxSize),
		EnvAuditMaxPercent: string(b.MaxPercent),
		EnvAuditBodyHead:   string(b.BodyHead),
	} {
		if value != "" {
			env[name] = value
		}
	}
}

// ConfigureAuditSpoolBudget overrides cfg's spool budget with whatever the
// pool container's environment sets, leaving the proxy's defaults for the rest.
// An unparsable value is an error, for the reason ConfiguredAuditRetention's
// is.
func ConfigureAuditSpoolBudget(cfg *proxy.RecordingConfig) error {
	if value := strings.TrimSpace(os.Getenv(EnvAuditMaxSize)); value != "" {
		size, err := ByteSize(value).Bytes()
		if err != nil {
			return fmt.Errorf("%s: %w", EnvAuditMaxSize, err)
		}
		cfg.MaxSpoolBytes = size
	}
	if value := strings.TrimSpace(os.Getenv(EnvAuditMaxPercent)); value != "" {
		percent, err := Percent(value).Value()
		if err != nil {
			return fmt.Errorf("%s: %w", EnvAuditMaxPercent, err)
		}
		cfg.MaxSpoolPercent = percent
	}
	if value := strings.TrimSpace(os.Getenv(EnvAuditBodyHead)); value != "" {
		head, err := ByteSize(value).Bytes()
		if err != nil {
			return fmt.Errorf("%s: %w", EnvAuditBodyHead, err)
		}
		if head <= 0 {
			return fmt.Errorf("%s must be greater than 0, got %s", EnvAuditBodyHead, value)
		}
		cfg.BodyHeadBytes = head
	}
	return nil
}

// ByteSize is a size in configuration, written as a number of bytes or with a
// unit ("64KiB", "100GiB", "5GB"), and validated when it is read.
type ByteSize string

// Bytes parses the size.
func (s ByteSize) Bytes() (int64, error) {
	size, err := humanize.ParseBytes(strings.TrimSpace(string(s)))
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", string(s), err)
	}
	if size > math.MaxInt64 {
		return 0, fmt.Errorf("size %q is too large", string(s))
	}
	return int64(size), nil
}

func (s *ByteSize) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("size must be a string such as %q: %w", "100GiB", err)
	}
	value = strings.TrimSpace(value)
	if value != "" {
		if _, err := ByteSize(value).Bytes(); err != nil {
			return err
		}
	}
	*s = ByteSize(value)
	return nil
}

// HeadSize is a ByteSize that must be greater than zero, refused when it is
// read rather than when a pool proxy starts with it.
type HeadSize string

func (s *HeadSize) UnmarshalJSON(data []byte) error {
	var size ByteSize
	if err := size.UnmarshalJSON(data); err != nil {
		return err
	}
	if size != "" {
		if n, _ := size.Bytes(); n <= 0 {
			return fmt.Errorf("body head %q must be greater than 0", string(size))
		}
	}
	*s = HeadSize(size)
	return nil
}

// Percent is a percentage in configuration, written with or without a trailing
// "%", within [0, 100], and validated when it is read.
type Percent string

// Value parses the percentage.
func (p Percent) Value() (float64, error) {
	value := strings.TrimSuffix(strings.TrimSpace(string(p)), "%")
	percent, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid percentage %q: %w", string(p), err)
	}
	if math.IsNaN(percent) || percent < 0 || percent > 100 {
		return 0, fmt.Errorf("percentage %q must be within [0, 100]", string(p))
	}
	return percent, nil
}

func (p *Percent) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("percentage must be a string such as %q: %w", "5%", err)
	}
	value = strings.TrimSpace(value)
	if value != "" {
		if _, err := Percent(value).Value(); err != nil {
			return err
		}
	}
	*p = Percent(value)
	return nil
}
