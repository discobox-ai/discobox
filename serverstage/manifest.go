// Package serverstage stages the server binary the CLI runs (ADR 0099).
//
// A release CLI does not carry the control plane; it carries a description of
// where to get it — a manifest naming, for this binary's own platform, every
// asset the server is made of, each with a URL and a SHA-256. Staging turns
// that description into files on disk: downloaded, hashed as they are written,
// and moved into place as a set only once every digest matches.
//
// One staged set per server version, so an upgrade lands beside what it
// replaces rather than over it — a server running out of its own directory is
// never overwritten underneath itself, and going back a version is a directory
// that is still there.
package serverstage

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Asset is one file the server is made of.
//
// The server is a single binary today and nothing about this says it stays
// one: a helper process, a firmware blob, a bundled frontend. Discovering that
// later is how a downloader grows a special case for "the other file", so the
// unit is a list from the start.
type Asset struct {
	// Name is the file's name inside the staged directory. A bare filename:
	// a manifest does not get to write outside the directory it is staged in.
	Name string `json:"name"`
	// URL is where the asset is downloaded from.
	URL string `json:"url"`
	// SHA256 is the digest the download must have, lowercase hex. It is what
	// makes the download trustworthy rather than the transport it arrived over.
	SHA256 string `json:"sha256"`
	// Executable marks an asset that is staged with the execute bit. The rest
	// are staged readable by this user and nothing else.
	Executable bool `json:"executable,omitempty"`
}

// Manifest describes the server assets for one platform and one version.
type Manifest struct {
	// Version names the staged set, and is the directory it lands in.
	Version string `json:"version"`
	// OS and Arch are the platform these assets are for, stated rather than
	// implied: a manifest is a file a user can pass, and staging the wrong
	// platform's server should fail while it is still a download rather than
	// as an exec format error minutes later.
	OS   string `json:"os"`
	Arch string `json:"arch"`
	// Command names the asset to execute. It is a name rather than a
	// convention so the file the server is called by can change without the
	// CLI learning about it.
	Command string  `json:"command"`
	Assets  []Asset `json:"assets"`
}

// DefaultManifest is the manifest a release build carries, as base64-encoded
// JSON. An ordinary build leaves it empty and has no server to download.
//
// Base64 because the value travels as a linker -X flag through a Taskfile
// through a shell, and JSON does not survive that trip in a form anybody wants
// to read in a diff.
//
// It describes this binary's own platform and no other. The release fans out
// natively (ADR 0066 §4), so no single link step can see every platform's
// digest — but the runner that builds a target's server is the one that links
// its CLI moments later, which is the only pairing that matters, because a CLI
// only ever needs a server for the machine it is running on.
var DefaultManifest = ""

// ErrNoManifest reports a build that carries no server download. Every
// development build is one.
var ErrNoManifest = errors.New("this build carries no server download")

// Default decodes the manifest linked into this binary.
func Default() (Manifest, error) {
	if strings.TrimSpace(DefaultManifest) == "" {
		return Manifest{}, ErrNoManifest
	}
	return DecodeManifest(DefaultManifest)
}

// HasDefault reports whether this build carries a server download at all,
// without decoding it. Resolution asks before it reports that there is nothing
// to run, so the answer can name the reason.
func HasDefault() bool { return strings.TrimSpace(DefaultManifest) != "" }

// DecodeManifest reads a base64-encoded manifest, as the linker carries one.
func DecodeManifest(encoded string) (Manifest, error) {
	encoded = strings.TrimSpace(encoded)
	// Padded or not, standard or URL alphabet: this crosses a shell and a
	// Taskfile, and failing a build over which base64 spelling reached the
	// linker would be a very poor error message for a very small difference.
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if decoded, err := encoding.DecodeString(encoded); err == nil {
			return ParseManifest(decoded)
		}
	}
	return Manifest{}, errors.New("decode server manifest: not valid base64")
}

// EncodeManifest is DecodeManifest's inverse, and is what the release build
// uses to produce the value it links in.
func EncodeManifest(m Manifest) (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// ParseManifest reads a manifest from JSON and checks that it describes
// something stageable.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	// A misspelled field is a manifest that does not say what its author
	// thought it said, and the failure it causes otherwise — a missing digest,
	// an asset staged without its execute bit — surfaces far from the typo.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("parse server manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

var (
	sha256Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
)

// Validate reports whether the manifest is one that can be staged safely.
func (m Manifest) Validate() error {
	if !versionPattern.MatchString(m.Version) {
		// The version is a directory name, so it is checked as one rather than
		// merely for emptiness: everything staged is addressed through it.
		return fmt.Errorf("server manifest version %q is not a usable directory name", m.Version)
	}
	if m.Command == "" {
		return errors.New("server manifest names no command to run")
	}
	if len(m.Assets) == 0 {
		return errors.New("server manifest lists no assets")
	}
	seen := make(map[string]bool, len(m.Assets))
	for _, asset := range m.Assets {
		if err := asset.validate(); err != nil {
			return err
		}
		if seen[asset.Name] {
			return fmt.Errorf("server manifest lists asset %q twice", asset.Name)
		}
		seen[asset.Name] = true
	}
	if !seen[m.Command] {
		return fmt.Errorf("server manifest names command %q, which is not one of its assets", m.Command)
	}
	return nil
}

func (a Asset) validate() error {
	if a.Name == "" {
		return errors.New("server manifest has an asset with no name")
	}
	// A bare filename. A manifest that could name ../ or an absolute path
	// would be staging files outside the directory it is being staged into,
	// which is the one thing verification cannot make safe.
	if a.Name != filepath.Base(a.Name) || a.Name == "." || a.Name == ".." ||
		strings.ContainsAny(a.Name, `/\`) || a.Name == manifestFileName {
		return fmt.Errorf("server manifest asset name %q is not a plain file name", a.Name)
	}
	if !sha256Pattern.MatchString(a.SHA256) {
		return fmt.Errorf("server manifest asset %q has no usable sha256 digest", a.Name)
	}
	parsed, err := url.Parse(a.URL)
	if err != nil {
		return fmt.Errorf("server manifest asset %q: %w", a.Name, err)
	}
	// Integrity is the digest's job, not the transport's, so plain HTTP is
	// accepted rather than refused — it is what a mirror on a build network or
	// a test server speaks. What is refused is a scheme this does not fetch at
	// all, which would otherwise fail as a confusing transport error.
	switch parsed.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("server manifest asset %q has URL scheme %q; expected http or https", a.Name, parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("server manifest asset %q has no host in its URL", a.Name)
	}
	return nil
}

// ForThisPlatform reports whether the manifest describes the machine it is
// being read on. A manifest that names another platform is still stageable —
// that is how a set is prepared for a machine other than this one — but it is
// not something to run here.
func (m Manifest) ForThisPlatform() bool {
	return m.OS == runtime.GOOS && m.Arch == runtime.GOARCH
}

// Platform is the manifest's target, for an error message to name.
func (m Manifest) Platform() string {
	if m.OS == "" && m.Arch == "" {
		return "an unstated platform"
	}
	return m.OS + "/" + m.Arch
}

// Dir is where the manifest's assets are staged under root.
func (m Manifest) Dir(root string) string { return filepath.Join(root, m.Version) }

// sameAssets reports whether two manifests describe the same files with the
// same contents. It is what makes a staged directory reusable: a version that
// was re-cut, or a --manifest naming different assets under a version already
// staged, is a different set under the same name and has to be staged again.
func (m Manifest) sameAssets(other Manifest) bool {
	if m.Command != other.Command || len(m.Assets) != len(other.Assets) {
		return false
	}
	digests := make(map[string]Asset, len(other.Assets))
	for _, asset := range other.Assets {
		digests[asset.Name] = asset
	}
	for _, asset := range m.Assets {
		staged, ok := digests[asset.Name]
		if !ok || staged.SHA256 != asset.SHA256 || staged.Executable != asset.Executable {
			return false
		}
	}
	return true
}
