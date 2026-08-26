// Package harness defines coding-harness contracts and built-in drivers.
package harness

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Session state values returned by SessionStateDeriver and by the generic,
// process-liveness-based fallback a caller uses when a harness has none.
const (
	SessionStateRunning    = "running"
	SessionStateIdle       = "idle"
	SessionStateNeedsInput = "needs_input"
	SessionStateExited     = "exited"
	SessionStateFailed     = "failed"
	SessionStateUnknown    = "unknown"
)

const (
	TerminalIDEnv = "DISCOBOX_TERMINAL_ID"
	SocketEnv     = "DISCOBOX_HOOK_SOCKET"
	// ImageLabel is the OCI image-config label containing the JSON-encoded,
	// non-secret image metadata (env, volumes, and the harness contract) used
	// when a harness image is registered.
	ImageLabel = "io.discobox.image.v1"

	// ReclaimLabel marks an image as one Discobox built and may therefore
	// delete once nothing uses it (ADR 0040). It is set by the pool-agent and
	// sandbox-agent Dockerfiles; harness images and anything else built FROM the
	// sandbox base inherit it through the image config, which is also what
	// carries it across a pull — a label cannot be added to an image after it
	// arrives.
	ReclaimLabel      = "io.discobox.reclaimable.v1"
	ReclaimLabelValue = "true"

	// ConfigureDir is the one directory the configure flow exchanges files in.
	// It is **not** /run/discobox itself: that holds the resolved secrets file
	// (ADR 0012 §3), the proxy's CA bundles and rendered trust env (ADR 0020),
	// and the control-plane and buildkit sockets, all root-owned. A configure
	// command runs as the sandbox user (ConfigureUserName), so it needs a
	// directory it can write, and widening /run/discobox to get one would let
	// that user replace any of those entries.
	//
	// sandbox-agent creates it in config mode, owned by the sandbox user and
	// mode 0700, so the widening stops at this directory (see
	// sandbox-agent/server → ensureConfigureDir).
	//
	// ConfigureOutputPath is where a harness's configure command writes the
	// secrets and files it collected, for the control plane to read back before
	// the ephemeral configure sandbox is deleted.
	//
	// ConfigurePreviousConfigPath is where the control plane seeds the previous
	// configuration before re-running configure, so a configure command may
	// pre-fill from it. It carries files and secret *metadata* only — never a
	// secret value.
	//
	// All three are fixed points of the image contract rather than per-image
	// settings — the configure commands hardcode them too.
	ConfigureDir                = "/run/discobox/configure"
	ConfigureOutputPath         = ConfigureDir + "/harness-configure.json"
	ConfigurePreviousConfigPath = ConfigureDir + "/harness-previous-config.json"

	// ConfigurePreviousEnvPrefix prefixes the environment variable carrying a
	// previously configured secret into the configure sandbox: a secret bound to
	// ANTHROPIC_API_KEY is offered back as PREV_ANTHROPIC_API_KEY.
	//
	// The value is a sentinel, not the credential: the proxy swaps it for the real
	// value on an outbound request only while a live grant covers it, so the
	// configure command can exercise the old credential without ever holding it.
	//
	// The prefix is deliberate. Seeding the original variable name would let the
	// harness CLI silently authenticate with the old credential, which would make
	// the configure flow's choice ambiguous and its verification meaningless.
	ConfigurePreviousEnvPrefix = "PREV_"

	// ConfigureUserName, ConfigureUserUID, and ConfigureUserGID are the account
	// the configure sandbox runs as. Unlike a run sandbox, whose user mirrors
	// the caller's own (ADR 0025 §5), a configure sandbox has no source and no
	// caller identity to mirror, so the flow names one itself. The image does
	// not carry this account; boot creates it (ADR 0025 §4).
	//
	// The point is that it is **not root**. A harness CLI is entitled to refuse
	// to run as root — Claude Code refuses `bypassPermissions` there — and a
	// configure sandbox that ran as root would be configuring the harness under
	// an identity no real sandbox ever uses, so a credential could verify here
	// and still be collected by a CLI that will not start later.
	ConfigureUserName = "discobox"
	ConfigureUserUID  = 10000
	ConfigureUserGID  = 10000
)

type Harness struct {
	ID      string
	TypeID  string
	Name    string
	Command []string
}

// Image describes the immutable harness behavior baked into one sandbox
// image. It is the harness sub-object of the ImageMetadata payload projected
// into ImageLabel, so the control plane can validate an image without
// downloading its filesystem layers. There is no separate baked-in file —
// image.json is only the build-time authoring source for the label.
type Image struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Description     string     `json:"description,omitempty"`
	RunCommand      []string   `json:"runCommand"`
	RelaunchCommand []string   `json:"relaunchCommand,omitempty"`
	Files           []File     `json:"files,omitempty"`
	Secrets         []Secret   `json:"secrets,omitempty"`
	Config          *ImageMode `json:"config,omitempty"`
}

// ImageMode describes the interactive configuration command supported by an
// image. The command writes its result to ConfigureOutputPath and may read the
// previous configuration from ConfigurePreviousConfigPath; both paths are part
// of the contract, so the image only declares the command.
type ImageMode struct {
	Command []string `json:"command"`
}

// Definition is a built-in shortcut for registering an included harness image.
type Definition struct {
	ID          string
	Name        string
	Description string
	Image       string
	Configure   *Configure
}

// Configure declares the provider resources and environment for an ephemeral
// configuration sandbox. The image supplies the configuration command and is
// expected to write ConfigureOutputPath before exiting.
type Configure struct {
	Image        string
	Env          map[string]string
	CPUVCPUs     float64
	MemoryBytes  int64
	StorageBytes int64
}

// File is a file to write into the harness's home directory when the harness is
// installed.
type File struct {
	Path       string
	Content    string
	CreateOnly bool
	Template   bool
}

// Secret declares an environment variable the harness expects, and whether it is
// required for the harness to run. Optional secrets are used when present but do
// not block the harness from launching.
//
// OneOfGroup ties a required secret to a set of alternatives: required secrets
// sharing a group form an at-least-one requirement, satisfied when any member is
// present (e.g. an API key or an OAuth token). Ungrouped required secrets each
// must be satisfied independently.
// Delivery says how the sandbox hands this credential to the harness. Empty
// (SecretDeliveryEnv) exports Name into the harness's environment. See
// SecretDeliveryFile for the other case.
type Secret struct {
	Name       string
	Required   bool
	OneOfGroup string
	Delivery   string
}

const (
	// SecretDeliveryEnv exports the secret's sentinel as the environment
	// variable the secret names. It is the default and the empty value.
	SecretDeliveryEnv = ""

	// SecretDeliveryFile means the harness reads this credential from a file
	// the harness config installs, and the variable is deliberately **not**
	// exported. The sentinel is still minted and still published to the
	// sandbox's secret map, so a templated file can place it (`.secrets.NAME`);
	// only the environment export is withheld.
	//
	// Withholding it is the point, not a tidiness preference. A CLI that reads
	// both prefers the variable, and a credential arriving that way carries
	// none of the metadata a file does — Claude Code, handed
	// CLAUDE_CODE_OAUTH_TOKEN, finds no `scopes` beside it and limits itself to
	// inference, refusing Remote Control, even when the file beside it says
	// otherwise. Exporting it would silently defeat the file.
	SecretDeliveryFile = "file"
)

type Driver interface {
	ID() string
	// Definition returns the harness's built-in harness-config template.
	Definition() Definition
}

// Converser is implemented by drivers that support automated multi-turn conversations.
// Prompt sends a user message and returns the final assistant response.
// state is an opaque blob from the previous call; nil starts a new conversation.
// The returned state must be passed to the next call to continue the conversation.
type Converser interface {
	Prompt(ctx context.Context, prompt string, state []byte) (result string, newState []byte, err error)
}

// HookRecord is one recorded harness lifecycle hook event, in the shape a
// caller (sandbox-agent) records them. It is a harness-package-local mirror
// so this package does not need to import the caller's store types.
type HookRecord struct {
	Event     string
	Payload   json.RawMessage
	CreatedAt time.Time
}

// SessionStateDeriver is implemented by harnesses that can compute a live
// session state (SessionStateRunning, SessionStateIdle, etc.) from their own
// recorded lifecycle hook events. It is optional, sibling to Converser:
// harnesses without one leave session-state derivation to the caller's
// generic, process-liveness-based fallback.
type SessionStateDeriver interface {
	// DeriveSessionState inspects hooks (ascending by CreatedAt; may be empty)
	// and returns the current session state, the most recent hook event name,
	// and when it occurred. An empty state means the deriver has no opinion
	// yet (e.g. no hooks recorded), and the caller should fall back to its
	// generic mapping instead.
	DeriveSessionState(hooks []HookRecord) (state, lastEvent string, lastEventAt time.Time)
}

func SetEnv(env map[string]string, key, value string) {
	if env == nil || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
		return
	}
	env[key] = value
}
