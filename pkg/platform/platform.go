// Package platform is what a sandbox runs on: an operating system and an
// architecture, spelled `os/arch` with Go's GOOS and GOARCH names, the way a
// server manifest (serverstage) and a guest image build already spell one.
//
// It is a placement key (ADR 0145 §1). A pool declares the one platform it
// hosts, and a sandbox runs on its pool's platform: a sandbox is placed only
// on a pool of its own platform, and a transfer never crosses one (ADR 0145
// §8).
//
// A pool also declares the kind of image it runs (ImageKind, ADR 26-10-09-106
// §4). A harness names neither: both are its pool's (ADR 26-11-01-122). In
// the root module because the control plane that places and the pool agent
// that declares must spell and compare it the same way.
package platform

import (
	"database/sql/driver"
	"fmt"
	"regexp"
	"runtime"
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

// Pool is the platform of a linux pool on this machine: Linux, whatever this
// machine runs, on this machine's architecture. Every container provider that
// runs a pool on this machine — docker, libkrun, vz, wslc — puts it on a Linux
// kernel of the host's architecture; a vm pool may name another OS for its
// sandboxes, darwin on vz, and declares it (ADR 26-11-11-963 §1). A pool
// elsewhere — a cloud VM, a remote Docker host — may be another architecture,
// and declares its own: this is never assumed of a pool, a sandbox, or an
// archive. It is only the platform whose image a harness's labels are read
// from when its image publishes it.
func Pool() Platform {
	return Platform{OS: Linux, Arch: runtime.GOARCH}
}

// Linux and Darwin are the operating systems a pool may name for its
// sandboxes when it is created (ADR 26-11-11-963 §1): Linux, the default,
// wherever the provider boots it, Darwin only where the provider's driver
// boots it.
const (
	Linux  = "linux"
	Darwin = "darwin"
)

// ParseSandboxOS reads the operating system a pool names for its sandboxes.
// The empty string is Linux, the default.
func ParseSandboxOS(s string) (string, error) {
	switch s = strings.TrimSpace(s); s {
	case "", Linux:
		return Linux, nil
	case Darwin:
		return Darwin, nil
	default:
		return "", fmt.Errorf("sandbox OS %q is neither %s nor %s", s, Linux, Darwin)
	}
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

func describe(p Platform) string {
	if p.IsZero() {
		return "no declared platform"
	}
	return p.String()
}
