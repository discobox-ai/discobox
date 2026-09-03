// Package harness defines coding-harness contracts and built-in drivers.
package harness

import "strings"

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

	// RunCommand is the command a harness image is expected to install on PATH,
	// and the one the runtime types when the image declares no override
	// (ADR 0086 §3). The base image ships it as a shim that execs nothing, so
	// an image installing no agent lands the user at a clean prompt.
	//
	// It is safe to type a command the image may not have because a harness
	// command is typed into a login shell rather than executed as argv
	// (ADR 0027): the shell reports what it could not find and hands back a
	// live prompt, so a wrong guess costs a line of output, not the terminal.
	//
	// Being typed is also why the prompt reaches the wrapper as words rather
	// than as one argument: the shell splits the line before the wrapper runs.
	// Rejoining everything after the flags with single spaces, and handing the
	// agent that one string, is the wrapper's half of the convention — an
	// agent CLI reads its prompt as a single positional and would otherwise
	// see only the first word.
	RunCommand = "discobox-harness-run"

	// ResumeFlag marks a relaunch — a terminal returning after its sandbox
	// stopped, or a revive in place (ADR 0038). What it means for the wrapped
	// agent is the wrapper's business: `--continue` for Claude Code, `resume
	// --last` for Codex, and nothing at all for an agent with no resume story.
	ResumeFlag = "--resume"
)

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
	Command  []string `json:"command"`
	Reminder string   `json:"reminder,omitempty"`
	// Ports are the callback ports the configure command needs reachable at
	// the same number on the user's own machine. See ConfigPort.
	Ports []ConfigPort `json:"ports,omitempty"`
}

// ConfigPort is one local port a configure command's sign-in depends on.
//
// A harness CLI signing in through a browser starts a callback server on its
// own localhost and sends the browser to a redirect URI naming that exact port
// — one the identity provider has registered in advance, so it is not
// negotiable. In a sandbox that server is on the *sandbox's* localhost, where
// the user's browser cannot reach it, which is why the only sign-in that has
// worked here is a device code.
//
// Declaring the port asks the configure flow to forward it: the same port
// number on this machine, tunneled into the configure sandbox, for as long as
// the flow runs. Same number or nothing — the redirect URI is fixed, so a
// forward that landed on the next free port would answer no browser while
// looking like it had worked.
type ConfigPort struct {
	Port int `json:"port"`
	// Unavailable is what to tell the user when the port cannot be bound here,
	// because something else on their machine already holds it. Only the image
	// knows what its harness can still do without the callback — sign in by
	// device code, in Codex's case — so the fallback is the image's to spell
	// out. Empty falls back to saying which port could not be bound.
	Unavailable string `json:"unavailable,omitempty"`
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

func SetEnv(env map[string]string, key, value string) {
	if env == nil || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
		return
	}
	env[key] = value
}
