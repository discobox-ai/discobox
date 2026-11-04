// Package harness defines coding-harness contracts and built-in drivers.
package harness

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/pkg/hostscope"
)

const (
	// ImageLabel is the OCI image-config label containing the JSON-encoded,
	// non-secret image metadata (env, volumes, and the harness contract) used
	// when a harness image is registered.
	ImageLabel = "io.discobox.image.v1"

	// ReclaimLabel marks an image as one Discobox built and may therefore
	// delete once nothing uses it (ADR 0040). It is set by the Docker
	// driver's sandbox base image, whose image config is also what carries
	// it across a pull — a label cannot be added to an image after it arrives
	// — and a pool sets it on the snapshots it commits. No bundle carries it.
	ReclaimLabel      = "io.discobox.reclaimable.v1"
	ReclaimLabelValue = "true"

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
	SaveBack        *harnesscapture.Definition `json:"saveBack,omitempty"`
	ID              string                     `json:"id"`
	Name            string                     `json:"name"`
	Description     string                     `json:"description,omitempty"`
	RunCommand      []string                   `json:"runCommand"`
	RelaunchCommand []string                   `json:"relaunchCommand,omitempty"`
	Files           []File                     `json:"files,omitempty"`
	Secrets         []Secret                   `json:"secrets,omitempty"`
	// EgressHosts are the destinations the harness needs to work at all — its
	// model API, chiefly — in the egress allow-entry form ("api.anthropic.com",
	// "*.example.com"). They join the harness level's allow list of every
	// restricted discobox on it, and never restrict anything themselves
	// (ADR 26-10-01-032 §3).
	EgressHosts []string `json:"egressHosts,omitempty"`
}

// retiredImageKeys are harness keys a bundle once declared for the configure
// flow, which is removed (ADR 26-11-02-152 §2). A bundle still declaring one
// is refused rather than having it silently ignored: its author expects a
// setup step that no longer runs.
var retiredImageKeys = map[string]string{
	"config": "the configure command and its ports",
}

// UnmarshalJSON refuses a harness declaration that still carries a retired
// configure key (retiredImageKeys).
func (i *Image) UnmarshalJSON(data []byte) error {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return err
	}
	for key, what := range retiredImageKeys {
		if _, ok := keys[key]; ok {
			return fmt.Errorf("harness declares %q (%s), which is retired: a harness is set up by signing in inside the box it runs in, and save-back shares it (ADR 26-11-02-152)", key, what)
		}
	}
	type plain Image
	return json.Unmarshal(data, (*plain)(i))
}

// Definition is a built-in shortcut for registering an included harness.
//
// Bundle is the harness bundle its manifest is read from and every box on it
// applies (ADR 26-10-16-423 §2).
type Definition struct {
	ID          string
	Name        string
	Description string
	Bundle      string
}

// File is a file to write into the harness's home directory when the harness is
// installed.
type File struct {
	Path       string
	Content    string
	CreateOnly bool
}

// UnmarshalJSON rejects retired executable file attributes in bundle metadata.
// A bundle must explicitly adopt literal files rather than silently delivering
// legacy template source as configuration.
func (f *File) UnmarshalJSON(data []byte) error {
	type plain File
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	return decoder.Decode((*plain)(f))
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
//
// RejectionScope names the requests that answer for this credential
// (ADR 26-10-13-595). A CLI sends one login to many endpoints, and they do not
// all refuse it for the same reason: an endpoint that turns a live login away
// for what it may do says nothing about whether it has to be signed in again.
// Only what an upstream makes of a request the scope names is taken as a fact
// about the credential, a refusal and an acceptance alike. Empty is every
// request.
//
// Routes are where the harness itself sends this credential (ADR
// 26-10-02-924, ADR 26-10-27-880 §4): a request carrying it must fall on one,
// and the most specific covering it must not be blocked. A route declared in
// the shape before host patterns reads as it was meant (harnesscapture.Route).
// Saving a harness change writes them onto the secret it creates; they are the
// image's alone, and a save cannot add to them or change them.
type Secret struct {
	Name           string
	Required       bool
	OneOfGroup     string
	Delivery       string
	RejectionScope []RejectionRule
	Routes         []harnesscapture.Route `json:",omitempty"`
}

// RejectionRule is one host of a Secret's RejectionScope, and the paths on it.
type RejectionRule struct {
	// Host covers itself and the hosts beneath it, the way a secret's host
	// binding does (hostscope).
	Host string `json:"host"`
	// Paths are prefixes on a segment boundary: "/v1/messages" covers
	// "/v1/messages" and "/v1/messages/batches", never "/v1/messagesX". The
	// query is not part of the match. Empty is every path on the host.
	Paths []string `json:"paths,omitempty"`
}

// Validate reports whether the rule names a host and paths a request can be
// matched against.
func (r RejectionRule) Validate() error {
	host := strings.ToLower(strings.TrimSpace(r.Host))
	if host == "" {
		// Not a wildcard: an entry that names no host is a scope that names
		// every request, which is what declaring no scope already says.
		return errors.New("a rejectionScope entry requires a host")
	}
	// A host and nothing else. A URL, a port or a path normalizes to a name no
	// destination has, and the scope would then silence every report.
	if strings.ContainsAny(host, "/:@?# \t") || hostscope.Normalize(host) != host {
		return fmt.Errorf("rejectionScope host %q must be a bare host name: no scheme, port or path", r.Host)
	}
	for _, path := range r.Paths {
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#") {
			return fmt.Errorf("rejectionScope path %q must be an absolute path with no query", path)
		}
	}
	return nil
}

const (
	// SecretDeliveryEnv exports the secret's sentinel as the environment
	// variable the secret names. It is the default and the empty value.
	SecretDeliveryEnv = ""

	// SecretDeliveryFile means the harness reads this credential from a file
	// the harness config installs, and the variable is deliberately **not**
	// exported. The sentinel is still minted and still published to the
	// sandbox's secret map, so a credential document can bind it by name;
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
