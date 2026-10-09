// Package platform is what a sandbox runs on: an operating system and an
// architecture, spelled `os/arch` with Go's GOOS and GOARCH names, the way a
// server manifest (serverstage) and a guest image build already spell one.
//
// It is a placement key (ADR 0145 §1). A pool declares the one platform it
// hosts, a harness config records the platforms its image is published for, and
// a sandbox runs on its pool's platform, which its harness must publish; a
// sandbox is placed only on a pool of its own platform, and a transfer never
// crosses one (ADR 0145 §8).
//
// The image kind (ImageKind) is the second placement key (ADR 26-10-09-106
// §4): a pool declares the kind of image it runs, a harness config records the
// kind its image is, and a sandbox is placed only where both the platform and
// the kind match. In the root module
// because the control plane that places, the pool agent that declares, and
// the CLI that offers only what a pool can run must all spell and compare it
// the same way.
package platform

import (
	"database/sql/driver"
	"fmt"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

// Platform is an operating system and an architecture. The zero value is no
// platform: one that has not been declared.
type Platform struct {
	OS   string
	Arch string
}

// namePattern is one GOOS or GOARCH name. It is what serverstage accepts for
// the same two fields, so a platform this package parses is one a release can
// name.
var namePattern = regexp.MustCompile(`^[a-z0-9]+$`)

// Current is the platform this process runs on.
func Current() Platform {
	return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
}

// Pool is the platform a pool on this machine hosts: Linux, whatever this
// machine runs, on this machine's architecture. Every provider that runs a pool
// on this machine — docker, libkrun, vz, wslc — puts it on a Linux kernel of the
// host's architecture. A pool elsewhere — a cloud VM, a remote Docker host —
// may be another architecture, and declares its own: this is never assumed of
// a pool, a sandbox, or an archive. It is only the platform whose image a
// harness's labels are read from when its image publishes it.
func Pool() Platform {
	return Platform{OS: "linux", Arch: runtime.GOARCH}
}

// Parse reads an `os/arch` platform. The empty string is the zero platform.
func Parse(s string) (Platform, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Platform{}, nil
	}
	os, arch, ok := strings.Cut(s, "/")
	p := Platform{OS: os, Arch: arch}
	if !ok {
		return Platform{}, fmt.Errorf("platform %q is not an os/arch pair", s)
	}
	if err := p.Validate(); err != nil {
		return Platform{}, err
	}
	return p, nil
}

// Validate reports whether p is a usable os/arch pair. The zero platform is
// not one.
func (p Platform) Validate() error {
	if !namePattern.MatchString(p.OS) || !namePattern.MatchString(p.Arch) {
		return fmt.Errorf("platform %q is not an os/arch pair", p.String())
	}
	return nil
}

// IsZero reports whether no platform has been declared.
func (p Platform) IsZero() bool { return p == Platform{} }

// String is the `os/arch` spelling, or the empty string for the zero platform.
func (p Platform) String() string {
	if p.IsZero() {
		return ""
	}
	return p.OS + "/" + p.Arch
}

// MarshalText writes the `os/arch` spelling, so JSON carries a platform as one
// string.
func (p Platform) MarshalText() ([]byte, error) {
	return []byte(p.String()), nil
}

// UnmarshalText reads what MarshalText writes.
func (p *Platform) UnmarshalText(text []byte) error {
	parsed, err := Parse(string(text))
	if err != nil {
		return err
	}
	*p = parsed
	return nil
}

// Value stores a platform as its `os/arch` spelling.
func (p Platform) Value() (driver.Value, error) {
	return p.String(), nil
}

// Scan reads a stored `os/arch` spelling.
func (p *Platform) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*p = Platform{}
		return nil
	case string:
		return p.UnmarshalText([]byte(v))
	case []byte:
		return p.UnmarshalText(v)
	default:
		return fmt.Errorf("cannot scan %T into a platform", src)
	}
}

// MismatchError is a sandbox refused by a pool that hosts another platform.
// The platforms are the reason, and the message says them both.
type MismatchError struct {
	Sandbox Platform
	Pool    Platform
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("this discobox runs on %s, and the pool hosts %s", describe(e.Sandbox), describe(e.Pool))
}

// Place reports whether a sandbox of platform sandbox may be placed on a pool
// that hosts pool: nil when they are the same platform, a *MismatchError when
// they are not. A platform that was never declared matches nothing, because
// there is nothing to say it would run.
func Place(sandbox, pool Platform) error {
	if sandbox.IsZero() || sandbox != pool {
		return &MismatchError{Sandbox: sandbox, Pool: pool}
	}
	return nil
}

// Set is the platforms an image is published for, sorted and without
// duplicates. An empty set is one nobody has read: it rules nothing out.
type Set []Platform

// NewSet builds a set from what an image publishes, dropping duplicates and
// anything that is not a usable os/arch pair — an index lists its attestation
// manifests as unknown/unknown.
func NewSet(platforms ...Platform) Set {
	out := make(Set, 0, len(platforms))
	for _, p := range platforms {
		if p.Validate() == nil && p.OS != "unknown" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b Platform) int { return strings.Compare(a.String(), b.String()) })
	return out
}

// Contains reports whether the set names p.
func (s Set) Contains(p Platform) bool { return slices.Contains(s, p) }

func (s Set) String() string {
	names := make([]string, len(s))
	for i, p := range s {
		names[i] = p.String()
	}
	return strings.Join(names, ", ")
}

// UnpublishedError is a pool whose platform a harness's image is not published
// for. An image built for one architecture — what a development build makes —
// runs only on a pool of that architecture, and the refusal says so rather
// than leaving the pool to fail pulling it.
type UnpublishedError struct {
	Published Set
	Pool      Platform
}

func (e *UnpublishedError) Error() string {
	return fmt.Sprintf("its image is published for %s only, and the pool hosts %s; an image built for one platform runs only on a pool of that platform",
		e.Published, describe(e.Pool))
}

// Publishes reports whether an image published for s runs on a pool that
// hosts pool: nil when s names it or when s is empty — an image nobody has
// read rules nothing out — and an *UnpublishedError otherwise.
func (s Set) Publishes(pool Platform) error {
	if len(s) == 0 || s.Contains(pool) {
		return nil
	}
	return &UnpublishedError{Published: s, Pool: pool}
}

func describe(p Platform) string {
	if p.IsZero() {
		return "no declared platform"
	}
	return p.String()
}
