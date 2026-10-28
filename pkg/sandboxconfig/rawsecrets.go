package sandboxconfig

import (
	"path"
	"time"
)

// A raw secret is a credential whose value a person handed the sandbox itself
// rather than a sentinel its egress exchanges (ADR 26-10-27-840). Its binding
// says how the sandbox's programs are given it.
const (
	// RawDeliveryEnv sets the variable to the value, in the environment of
	// each process the sandbox agent starts, and nowhere it keeps.
	RawDeliveryEnv = "env"
	// RawDeliveryFile sets the variable to the path of a file under
	// RawSecretsDir holding the value.
	RawDeliveryFile = "file"
)

// ValidRawDelivery reports whether delivery is one a raw binding may name.
// Empty is no raw delivery: a sentinel.
func ValidRawDelivery(delivery string) bool {
	return delivery == RawDeliveryEnv || delivery == RawDeliveryFile
}

// RawSecretsDir is where the sandbox agent keeps raw values: on the sandbox's
// own /run, a tmpfs, never on a volume or a mount from its pool, so a value
// is never written to a disk (ADR 26-10-27-840 §3).
const RawSecretsDir = "/run/discobox/raw-secrets" //nolint:gosec // A directory, not a credential.

// RawSecretPath is the file a raw secret delivered in variable name is kept
// in.
func RawSecretPath(name string) string {
	return path.Join(RawSecretsDir, name)
}

// RawSecretsRoute is the pool credentials endpoint's route the sandbox agent
// reads its raw secrets from, over its own mTLS link to the pool. It is not
// part of the agent credentials protocol, which never carries a value, and the
// sandbox's credentials relay does not serve it.
const RawSecretsRoute = "/v1/raw-secrets" //nolint:gosec // A route, not a credential.

// RawSecret is one raw secret as the pool hands it to its sandbox: the
// variable it is delivered in, how, and the value as it stands. ExpiresAt is
// when the value or its binding lapses, when it does; the sandbox asks again
// before then.
type RawSecret struct {
	Name      string     `json:"name"`
	Delivery  string     `json:"delivery"`
	Value     string     `json:"value"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// RawSecretsResponse is what RawSecretsRoute answers with: every raw secret
// the sandbox holds now. One it no longer holds is absent.
type RawSecretsResponse struct {
	Secrets []RawSecret `json:"secrets"`
}
