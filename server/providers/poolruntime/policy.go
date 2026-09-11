package poolruntime

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// PoolPolicy is the configuration every pool provider instance carries,
// whatever backend it runs its pools on.
//
// It is embedded anonymously into each provider's own Config so its fields
// flatten into that provider's JSON, on the same terms as PoolManifest in
// model.Pool: declared once, and impossible for a backend to be quietly
// missing. Everything here describes how a pool treats what it holds — its own
// disk, and the sandboxes it hosts — which is a property of pools rather than of
// the machine one happens to run on. The settings that do belong to a backend
// (image, region, socket) stay in the provider's own Config.
type PoolPolicy struct {
	// ProxyAuditRetention is how long the pool proxy keeps an audit row and the
	// request/response body or upgraded-stream capture it names. Empty leaves
	// the pool proxy on proxy.DefaultRetention.
	//
	// Nothing else reclaims those trees. Deleting a sandbox deliberately does
	// not: what its sandbox sent is the question the audit trail exists to
	// answer, and it is most often asked after the sandbox is gone.
	ProxyAuditRetention Duration `json:"proxyAuditRetention,omitempty"`
	// SandboxIdleTimeout is how long a sandbox on this provider's pools runs
	// with nothing happening in it — no terminal title changing, no client
	// connected, no keepalive lease — before it powers itself off (ADR 0108).
	// The next use starts it again. Empty leaves every sandbox on the
	// sandbox-agent's default.
	SandboxIdleTimeout Duration `json:"sandboxIdleTimeout,omitempty"`
}

// defaultRetentionHint mirrors proxy.DefaultRetention for display only. It is
// written out rather than imported because the pool proxy lives in the root
// module and pulls a whole HTTP proxy stack behind it, which the workspace
// would then propagate into every module that reaches this one — a lot of
// dependency for a placeholder in a form. Nothing reads it: the actual default
// is applied by the proxy itself when this setting is left empty, so the two
// drifting costs a stale hint and nothing more.
const defaultRetentionHint = "48h"

// defaultIdleTimeoutHint mirrors autostop.DefaultIdleTimeout for display only,
// on the same terms as defaultRetentionHint: the sandbox-agent applies the
// real default when this is left empty, and the sandbox-agent module is not
// one the server builds against.
const defaultIdleTimeoutHint = "30m"

// PoolPolicyConfigFields describes PoolPolicy for provider catalogs. Every
// provider definition appends it, so the fields appear identically wherever a
// provider instance is configured.
func PoolPolicyConfigFields() []sandbox.ProviderConfigField {
	return []sandbox.ProviderConfigField{
		{
			Key:         "proxyAuditRetention",
			Label:       "Proxy Audit Retention",
			Type:        "string",
			Description: "How long the pool proxy keeps request audit records and recorded bodies, as a Go duration.",
			Placeholder: defaultRetentionHint,
			Advanced:    true,
		},
		{
			Key:         "sandboxIdleTimeout",
			Label:       "Sandbox Idle Timeout",
			Type:        "string",
			Description: "How long a sandbox runs with nothing happening in it — no terminal title changing, no client connected, no keepalive lease — before it stops itself, as a Go duration. The next use starts it again.",
			Placeholder: defaultIdleTimeoutHint,
			Advanced:    true,
		},
	}
}

// Duration is a time.Duration in provider configuration, accepted as a Go
// duration string ("48h", "30m").
//
// It is a distinct type because time.Duration marshals as a bare nanosecond
// count, which is neither what anyone would type into a configuration field nor
// what they would expect to read back out of one.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("duration must be a string such as %q: %w", defaultRetentionHint, err)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value, err)
	}
	if parsed < 0 {
		return fmt.Errorf("duration %q cannot be negative", value)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	if d == 0 {
		return []byte(`""`), nil
	}
	return json.Marshal(time.Duration(d).String())
}

// Value returns the configured window, or zero when it was left unset.
func (d Duration) Value() time.Duration { return time.Duration(d) }
