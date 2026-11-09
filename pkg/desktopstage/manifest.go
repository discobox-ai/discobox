// Package desktopstage stages the desktop app `discobox desktop` runs (ADR
// 26-11-07-983 §2).
//
// A release CLI does not carry the app. It carries a manifest describing the
// app for its own platform: one archive, its URLs, its size and its SHA-256.
// Staging downloads the archive, verifies the digest, and unpacks it into a
// directory named by that digest, under a staging root private to this user
// (never a shared cache: see Options.Root). A CLI release's app lands beside
// every other one, never over a file that may be running, and a digest no
// running app uses is pruned.
//
// The asset and its verified download are serverstage's: the app is pinned
// exactly as the server is. What differs is what happens once the bytes
// verify: the app is an archive (a macOS app bundle is a directory), so it is
// unpacked, and a directory is named by content rather than by version.
package desktopstage

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"runtime"
	"strings"

	"github.com/discobox-ai/discobox/pkg/serverstage"
)

// Manifest describes the desktop app for one platform and one release.
type Manifest struct {
	// Version is the release the app belongs to. It is reported, not used to
	// name anything: the staged directory is the asset's digest.
	Version string `json:"version"`
	// OS and Arch are the platform the app is for, stated so that a manifest
	// for another machine fails as a refusal rather than an exec format error.
	OS   string `json:"os"`
	Arch string `json:"arch"`
	// Command is the executable to run, as a slash-separated path inside the
	// unpacked archive (`discobox-desktop`, or a bundle's
	// `discobox.app/Contents/MacOS/discobox`).
	Command string `json:"command"`
	// Asset is the archive, a gzip-compressed tar.
	Asset serverstage.Asset `json:"asset"`
}

// DefaultManifest is the manifest a release build links in, as base64 JSON,
// beside serverstage.DefaultManifest. A development build leaves it empty and
// finds the app beside itself instead.
var DefaultManifest = ""

// ErrNoManifest reports a build that carries no desktop app download.
var ErrNoManifest = errors.New("this build carries no desktop app download")

// Default decodes the manifest linked into this binary.
func Default() (Manifest, error) {
	if strings.TrimSpace(DefaultManifest) == "" {
		return Manifest{}, ErrNoManifest
	}
	return DecodeManifest(DefaultManifest)
}

// DecodeManifest reads a base64-encoded manifest, as the linker carries one.
// Any base64 spelling is accepted, for serverstage.DecodeManifest's reason: it
// crosses a shell and a Taskfile on its way to the linker.
func DecodeManifest(encoded string) (Manifest, error) {
	encoded = strings.TrimSpace(encoded)
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if decoded, err := encoding.DecodeString(encoded); err == nil {
			return ParseManifest(decoded)
		}
	}
	return Manifest{}, errors.New("decode desktop manifest: not valid base64")
}

// EncodeManifest is DecodeManifest's inverse: what the release build links in.
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

// ParseManifest reads a manifest from JSON and checks it can be staged.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("parse desktop manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

var (
	versionPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	platformPattern = regexp.MustCompile(`^[a-z0-9]+$`)
)

// Validate reports whether the manifest is one that can be staged safely.
func (m Manifest) Validate() error {
	if !versionPattern.MatchString(m.Version) {
		return fmt.Errorf("desktop manifest version %q is not a release name", m.Version)
	}
	if !platformPattern.MatchString(m.OS) || !platformPattern.MatchString(m.Arch) {
		return fmt.Errorf("desktop manifest names platform %q, which is not a GOOS/GOARCH pair", m.Platform())
	}
	if err := checkMemberPath(m.Command); err != nil {
		return fmt.Errorf("desktop manifest command: %w", err)
	}
	if err := m.Asset.Validate(); err != nil {
		return fmt.Errorf("desktop manifest: %w", err)
	}
	return nil
}

// checkMemberPath accepts a slash-separated relative path that stays inside
// the directory it is joined to: no absolute path, no `..`, nothing to clean.
func checkMemberPath(name string) error {
	if name == "" {
		return errors.New("no path")
	}
	if strings.Contains(name, `\`) || path.IsAbs(name) || path.Clean(name) != name ||
		name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return fmt.Errorf("%q is not a plain relative path", name)
	}
	return nil
}

// ForThisPlatform reports whether the manifest describes the machine it is
// read on.
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
