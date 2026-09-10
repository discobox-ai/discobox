package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
)

// ImageAPIVersion is the contract version for the payload carried by
// ImageLabel.
const ImageAPIVersion = "discobox.dev/image/v1"

// ImageMetadata is the full non-secret payload a harness image declares: env
// defaults, declarative volumes, supplementary OS groups the sandbox user
// needs, and the harness contract. It is the sole carrier of image-owned
// data, projected into ImageLabel; there is no separate baked-in file for it.
type ImageMetadata struct {
	APIVersion string            `json:"apiVersion"`
	Env        map[string]string `json:"env,omitempty"`
	Volumes    []Volume          `json:"volumes,omitempty"`
	// AdditionalGroups names OS groups (already present in the image, e.g.
	// "docker" from the docker-ce package) the sandbox user is added to at
	// boot, alongside its own primary group. An image needs this when a
	// tool it ships (like the Docker CLI) checks group membership rather
	// than relying solely on the sudo access every sandbox user already has.
	//
	// A group here grants access only when its GID inside the image matches
	// the one on the host object it guards — a sandbox container is
	// privileged, so it sees the host's /var/run/docker.sock and /dev/kvm with
	// the host's numeric owners. Both sides are the same Debian release
	// installing the same packages, so the allocations agree; a group whose
	// GID diverges is membership in a group that owns nothing, which fails as
	// a permission error rather than as anything louder.
	AdditionalGroups []string `json:"additionalGroups,omitempty"`
	Harness          *Image   `json:"harness,omitempty"`
}

// VolumeKind selects which primary volume backs a declared path.
type VolumeKind string

const (
	VolumeData  VolumeKind = "data"
	VolumeCache VolumeKind = "cache"
)

// VolumeScope says who a cache path is shared with. It exists because sharing a
// cache directory is safe exactly when nothing in it is owned by one particular
// user, and only the image knows which of its paths are like that (ADR 0094).
//
// The default is deliberately the safe one: a path that says nothing is scoped
// to the sandbox user, because that is what a cache directory filled by the
// sandbox user needs, and because an image that forgets to think about this at
// all should not get the answer that hands one user's files to another. Sharing
// is the claim that has to be made out loud.
type VolumeScope string

const (
	// VolumeScopeUser gives each sandbox user their own copy of the path. It is
	// what an unset scope means.
	VolumeScopeUser VolumeScope = "user"
	// VolumeScopeShared puts every sandbox in the pool on one directory,
	// whoever they run as. Only correct where nothing below the path belongs to
	// a user: /nix is root-owned, world-readable, content-addressed, and reached
	// through a root daemon, so two uids on one store is what nix is built for.
	VolumeScopeShared VolumeScope = "shared"
)

// Volume is an image-declared path the sandbox-agent wires from a primary
// volume during boot. Path may contain the %HOME% token; UID and GID accept
// either a JSON number or a runtime token (%UID%/%GID%); Mode is an octal
// string (e.g. "0755"). All are resolved at mount time via ResolveVolumes.
//
// Scope applies to cache paths only and defaults to VolumeScopeUser. It is not
// derivable from UID: that field says who owns the mountpoint, and a path that
// declares no owner at all is still filled by the sandbox user -- so inferring
// "shareable" from "uid 0" would make the unstated case the dangerous one.
type Volume struct {
	Path   string      `json:"path"`
	Volume VolumeKind  `json:"volume"`
	Scope  VolumeScope `json:"scope,omitempty"`
	UID    ScalarToken `json:"uid,omitempty"`
	GID    ScalarToken `json:"gid,omitempty"`
	Mode   string      `json:"mode,omitempty"`
}

// ScalarToken holds a JSON scalar that is either an integer literal or a token
// string. An empty value means the field was omitted.
type ScalarToken string

func (s *ScalarToken) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*s = ""
		return nil
	}
	if b[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		*s = ScalarToken(str)
		return nil
	}
	*s = ScalarToken(string(b))
	return nil
}

func (s ScalarToken) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(s))
}

// VolumeRuntime carries the runtime identity used to expand volume tokens.
type VolumeRuntime struct {
	Home string
	UID  int
	GID  int
}

// ResolvedVolume is a Volume with all tokens expanded and fields parsed. Scope
// is concrete here -- never empty -- so nothing downstream has to know what an
// unset scope meant.
type ResolvedVolume struct {
	Path  string
	Kind  VolumeKind
	Scope VolumeScope
	UID   *int
	GID   *int
	Mode  *os.FileMode
}

// ResolveVolumes expands every declared volume's tokens against the runtime
// identity and validates the result.
func ResolveVolumes(volumes []Volume, rt VolumeRuntime) ([]ResolvedVolume, error) {
	if len(volumes) == 0 {
		return nil, nil
	}
	out := make([]ResolvedVolume, 0, len(volumes))
	for idx, v := range volumes {
		// A volume path is a path inside the sandbox, so it is a Linux path on
		// every host and is judged as one. filepath here read "/home/ada/.cache"
		// as relative on a Windows host and refused every declared volume — the
		// control plane resolves these, and it runs wherever the user does.
		volumePath := expandVolumeToken(v.Path, rt)
		if strings.TrimSpace(volumePath) == "" {
			return nil, fmt.Errorf("volume[%d]: path is required", idx)
		}
		if !path.IsAbs(volumePath) {
			return nil, fmt.Errorf("volume %q: path must be absolute", volumePath)
		}
		switch v.Volume {
		case VolumeData, VolumeCache:
		default:
			return nil, fmt.Errorf("volume %q: unknown volume kind %q", volumePath, v.Volume)
		}
		scope, err := resolveScope(v.Volume, v.Scope)
		if err != nil {
			return nil, fmt.Errorf("volume %q: %w", volumePath, err)
		}
		rv := ResolvedVolume{Path: path.Clean(volumePath), Kind: v.Volume, Scope: scope}
		if uid, ok, err := resolveScalar(v.UID, rt); err != nil {
			return nil, fmt.Errorf("volume %q uid: %w", volumePath, err)
		} else if ok {
			rv.UID = &uid
		}
		if gid, ok, err := resolveScalar(v.GID, rt); err != nil {
			return nil, fmt.Errorf("volume %q gid: %w", volumePath, err)
		} else if ok {
			rv.GID = &gid
		}
		if mode := strings.TrimSpace(v.Mode); mode != "" {
			parsed, err := strconv.ParseUint(mode, 8, 32)
			if err != nil {
				return nil, fmt.Errorf("volume %q mode %q: %w", volumePath, mode, err)
			}
			m := os.FileMode(parsed)
			rv.Mode = &m
		}
		out = append(out, rv)
	}
	return out, nil
}

// ValidateVolumeScope reports whether a declared scope can be honored for this
// volume kind, without needing the runtime identity ResolveVolumes wants.
//
// It is separate so the control plane can reject a bad scope where it reads the
// image label, naming the image, rather than letting it through to fail at boot
// four layers away -- the same reason the kind is checked there too.
//
// A shared data path is refused rather than ignored: a data volume is one
// sandbox's own tree, so nothing could carry out the claim, and an image that
// makes it has misunderstood which volume it declared. Honoring it silently
// would leave the image believing in sharing that never happens.
func ValidateVolumeScope(kind VolumeKind, scope VolumeScope) error {
	switch scope {
	case "", VolumeScopeUser:
		return nil
	case VolumeScopeShared:
		if kind != VolumeCache {
			return fmt.Errorf("scope %q applies to cache paths only", scope)
		}
		return nil
	default:
		return fmt.Errorf("unknown scope %q", scope)
	}
}

// resolveScope validates and then fills in the default, so an unset scope
// arrives downstream as the user scope it means.
func resolveScope(kind VolumeKind, scope VolumeScope) (VolumeScope, error) {
	if err := ValidateVolumeScope(kind, scope); err != nil {
		return "", err
	}
	if scope == "" {
		return VolumeScopeUser, nil
	}
	return scope, nil
}

func resolveScalar(tok ScalarToken, rt VolumeRuntime) (int, bool, error) {
	s := strings.TrimSpace(string(tok))
	if s == "" {
		return 0, false, nil
	}
	switch s {
	case "%UID%":
		return rt.UID, true, nil
	case "%GID%":
		return rt.GID, true, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false, fmt.Errorf("invalid numeric or token value %q", s)
	}
	return n, true, nil
}

func expandVolumeToken(value string, rt VolumeRuntime) string {
	replacer := strings.NewReplacer(
		"%HOME%", rt.Home,
		"%UID%", strconv.Itoa(rt.UID),
		"%GID%", strconv.Itoa(rt.GID),
	)
	return replacer.Replace(value)
}

// HomeToken is the placeholder an image-declared env value uses for the
// sandbox user's home directory. It survives into sandbox.json whenever the
// pool agent cannot resolve that home, and the sandbox expands it on the way
// into a process environment -- the same treatment sandboxconfig.LocalSubnetsToken
// gets for the same reason (ADR 0033 §5).
const HomeToken = "%HOME%"

// ExpandEnvHomeTokens replaces HomeToken in every value of an image-declared
// env map with the sandbox user's home directory.
//
// An empty home leaves the token in place rather than substituting a blank.
// The pool agent only knows the home when the request stated it outright; the
// account otherwise lives in the image, where only the sandbox can look it up.
// Expanding to "" there would turn "$HOME/.config" into "/.config" -- a real
// path, pointing at the wrong place, indistinguishable downstream from one
// somebody meant.
func ExpandEnvHomeTokens(env map[string]string, home string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for key, value := range env {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if home == "" {
			out[key] = value
			continue
		}
		out[key] = strings.ReplaceAll(value, HomeToken, home)
	}
	return out
}
