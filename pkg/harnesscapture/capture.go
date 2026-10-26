// Package harnesscapture defines the versioned protocol between a harness's
// capture commands, the sandbox that observes them, and the control plane that
// accepts a user's save. Secret-bearing objects must never be logged or sent to
// a client; Preview is the public projection.
package harnesscapture

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
)

const Version = 1
const MaxBytes = 2 << 20
const ScopeRead = "harness-config:read"
const ScopeCapture = "harness-config:capture"
const ScopeWrite = "harness-config:write"

// Definition is registered from bundle metadata, never from a sandbox export.
type Definition struct {
	Version            int        `json:"version"`
	Watch              []string   `json:"watch"`
	ExportCommand      []string   `json:"exportCommand"`
	HandoffCommand     []string   `json:"handoffCommand"`
	Providers          []Provider `json:"providers"`
	Settings           []Setting  `json:"settings"`
	CredentialSettings []Setting  `json:"credentialSettings,omitempty"`
	CredentialFiles    []string   `json:"credentialFiles,omitempty"`
}

type Provider struct {
	ID               string   `json:"id"`
	EnvName          string   `json:"envName"`
	MultipleAccounts bool     `json:"multipleAccounts,omitempty"`
	Types            []string `json:"types"`
	Host             string   `json:"host,omitempty"`
	OAuth            *OAuth   `json:"oauth,omitempty"`
	Routes           []Route  `json:"routes,omitempty"`
}

type OAuth struct {
	TokenURL string `json:"tokenUrl"`
	ClientID string `json:"clientId"`
	Encoding string `json:"encoding,omitempty"`
}

type Route struct {
	Host string `json:"host"`
	Path string `json:"path,omitempty"`
	Use  string `json:"use,omitempty"`
}

// Setting lists the portable fields a harness understands. Fields are dotted
// object paths and their scalar types (string, number, boolean, string-array).
// Structured settings need an explicitly named leaf, not an arbitrary object.
type Setting struct {
	TemplateSuffix string            `json:"templateSuffix,omitempty"`
	Path           string            `json:"path"`
	Format         string            `json:"format"`
	Fields         map[string]string `json:"fields"`
	CreateOnly     bool              `json:"createOnly,omitempty"`
}

type File struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	CreateOnly bool   `json:"createOnly,omitempty"`
	Template   bool   `json:"template,omitempty"`
}

// Credential carries newly observed material or a reference to a delivered
// binding. LocalID and Generation identify the exact entry handoff may replace.
type Credential struct {
	Provider   string          `json:"provider"`
	EnvName    string          `json:"envName"`
	Account    string          `json:"account,omitempty"`
	LocalID    string          `json:"localId,omitempty"`
	Generation string          `json:"generation,omitempty"`
	Type       string          `json:"type"`
	Keep       bool            `json:"keep,omitempty"`
	Value      json.RawMessage `json:"value,omitempty"`
}

type Diagnostic struct {
	Path    string `json:"path,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Export struct {
	Version     int          `json:"version"`
	Files       []File       `json:"files"`
	Credentials []Credential `json:"credentials"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
	// ProtectedPaths cannot be restored while their login is unsupported or
	// incompletely written. The watcher preserves them until the user resolves it.
	ProtectedPaths []string `json:"protectedPaths,omitempty"`
}

// Baseline's revisions name what was delivered, not the latest server state.
type Baseline struct {
	HarnessConfigID string            `json:"harnessConfigId"`
	ConfigRevision  int64             `json:"configRevision"`
	ManifestDigest  string            `json:"manifestDigest"`
	SecretRevisions map[string]string `json:"secretRevisions,omitempty"`
	Files           []File            `json:"files,omitempty"`
	ProjectFiles    []string          `json:"projectFiles,omitempty"`
}

// ExportInput is private input to the bundle-owned command. BaselineOnly asks
// it to normalize delivered files rather than read current settings from disk.
type ExportInput struct {
	KnownSentinels map[string][]string `json:"knownSentinels,omitempty"`
	Reconsider     *ReconsiderInput    `json:"reconsider,omitempty"`
	Version        int                 `json:"version"`
	Definition     Definition          `json:"definition"`
	Baseline       Baseline            `json:"baseline"`
	BaselineOnly   bool                `json:"baselineOnly,omitempty"`
	Sentinels      map[string]string   `json:"sentinels"`
	WorkingDir     string              `json:"workingDir"`
}

type FileChange struct {
	Path   string `json:"path"`
	Remove bool   `json:"remove,omitempty"`
	File   *File  `json:"file,omitempty"`
}

// Candidate is immutable and private. Its ID is random, not a public hash of
// secret material. WriteRevision may advance through explicit reconsideration;
// Baseline continues to describe the content the sandbox actually received.
type Candidate struct {
	ID              string            `json:"id"`
	Baseline        Baseline          `json:"baseline"`
	WriteRevision   int64             `json:"writeRevision"`
	SecretRevisions map[string]string `json:"secretRevisions,omitempty"`
	Files           []FileChange      `json:"files"`
	Credentials     []Credential      `json:"credentials"`
	Diagnostics     []Diagnostic      `json:"diagnostics,omitempty"`
}

type CredentialPreview struct {
	Provider string `json:"provider"`
	EnvName  string `json:"envName"`
	Account  string `json:"account,omitempty"`
	Type     string `json:"type"`
}

type Preview struct {
	CandidateID     string              `json:"candidateId,omitempty"`
	HarnessConfigID string              `json:"harnessConfigId"`
	ConfigRevision  int64               `json:"configRevision"`
	State           string              `json:"state"`
	Files           []FileChange        `json:"files"`
	Credentials     []CredentialPreview `json:"credentials"`
	Diagnostics     []Diagnostic        `json:"diagnostics,omitempty"`
}

// Handoff is delivered only after the server has committed this candidate.
// Files are the accepted credential templates rendered with Sentinels by the
// sandbox, never resolved values from the server.
type Handoff struct {
	Version     int               `json:"version"`
	CandidateID string            `json:"candidateId"`
	Credentials []Credential      `json:"credentials"`
	Files       []File            `json:"files"`
	Sentinels   map[string]string `json:"sentinels"`
}

type HandoffResult struct {
	State string `json:"state"` // applied, local-conflict, retryable-failure
}

var envPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func HomePath(p string) bool {
	return p != "" && p != "." && !strings.ContainsAny(p, "\\\x00") && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../")
}

func (d Definition) Validate() error {
	if d.Version != Version {
		return fmt.Errorf("unsupported capture version %d", d.Version)
	}
	if len(d.Watch) == 0 || len(d.Watch) > 64 {
		return errors.New("capture requires 1 to 64 watch inputs")
	}
	for _, p := range append(slices.Clone(d.Watch), d.CredentialFiles...) {
		if !HomePath(p) {
			return fmt.Errorf("invalid capture path %q", p)
		}
	}
	for _, argv := range [][]string{d.ExportCommand, d.HandoffCommand} {
		if len(argv) == 0 || len(argv) > 32 || !strings.HasPrefix(argv[0], "/") {
			return errors.New("capture commands require an absolute executable path")
		}
		for _, arg := range argv {
			if strings.ContainsRune(arg, 0) {
				return errors.New("capture command contains NUL")
			}
		}
	}
	seen := map[string]bool{}
	for _, p := range d.Providers {
		if p.ID == "" || seen[p.ID] || !envPattern.MatchString(p.EnvName) {
			return errors.New("invalid or duplicate capture provider")
		}
		seen[p.ID] = true
		if len(p.Types) == 0 {
			return errors.New("capture provider has no credential types")
		}
		for _, t := range p.Types {
			if t != "token" && t != "oauth" {
				return errors.New("unsupported capture credential type")
			}
		}
		if slices.Contains(p.Types, "oauth") && p.OAuth == nil {
			return errors.New("OAuth capture provider has no refresh definition")
		}
		if p.OAuth != nil {
			u, e := url.Parse(p.OAuth.TokenURL)
			if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || p.OAuth.ClientID == "" {
				return errors.New("invalid OAuth capture provider")
			}
			if p.OAuth.Encoding != "" && p.OAuth.Encoding != "form" {
				return errors.New("unsupported OAuth encoding")
			}
		}
	}
	seen = map[string]bool{}
	for _, f := range append(slices.Clone(d.Settings), d.CredentialSettings...) {
		if !HomePath(f.Path) || seen[f.Path] {
			return errors.New("invalid or duplicate capture settings path")
		}
		seen[f.Path] = true
		if f.Format != "json" && f.Format != "jsonc" && f.Format != "toml" && f.Format != "yaml" {
			return errors.New("unsupported capture settings format")
		}
		for field, t := range f.Fields {
			if field == "" || strings.Contains(field, "..") {
				return errors.New("invalid capture field")
			}
			if !slices.Contains([]string{"string", "number", "boolean", "string-array", "secret", "placeholder", "secret-or-placeholder", "secret-or-null", "id-token"}, t) {
				return errors.New("unsupported capture field type")
			}
		}
	}
	return nil
}

func (d Definition) Provider(id, env string) (Provider, bool) {
	for _, p := range d.Providers {
		if p.ID != id {
			continue
		}
		if env == p.EnvName {
			return p, true
		}
		if p.MultipleAccounts && strings.HasPrefix(env, p.EnvName+"_") {
			suffix := strings.TrimPrefix(env, p.EnvName+"_")
			if suffix != "" && strings.Trim(suffix, "0123456789") == "" {
				return p, true
			}
		}
	}
	return Provider{}, false
}

// Selection is the user's explicit decision after seeing a conflict. Entries
// not selected are left at their remote values; no secret value travels here.
type Selection struct {
	CandidateID        string   `json:"candidateId"`
	KeepFiles          []string `json:"keepFiles"`
	ReplaceCredentials []string `json:"replaceCredentials"`
}

type Reconsideration struct {
	Selection Selection `json:"selection"`
	Current   Baseline  `json:"current"`
}

type Acknowledgment struct {
	Superseded  bool              `json:"superseded,omitempty"`
	CandidateID string            `json:"candidateId"`
	Baseline    Baseline          `json:"baseline"`
	Files       []File            `json:"files"`
	Sentinels   map[string]string `json:"sentinels"`
}

type SaveResult struct {
	CandidateID     string `json:"candidateId"`
	HarnessConfigID string `json:"harnessConfigId"`
	ConfigRevision  int64  `json:"configRevision"`
	State           string `json:"state"`
}

type CandidateRequest struct {
	CandidateID string `json:"candidateId"`
}

// ReconsiderInput asks the harness to reconstruct credential delivery using only
// the explicitly selected immutable credentials and the current server bindings.
type ReconsiderInput struct {
	Candidate          Candidate `json:"candidate"`
	Current            Baseline  `json:"current"`
	ReplaceCredentials []string  `json:"replaceCredentials"`
}
