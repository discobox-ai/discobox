package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	AdditionalGroups []string `json:"additionalGroups,omitempty"`
	Harness          *Image   `json:"harness,omitempty"`
}

// VolumeKind selects which primary volume backs a declared path.
type VolumeKind string

const (
	VolumeData  VolumeKind = "data"
	VolumeCache VolumeKind = "cache"
)

// Volume is an image-declared path the sandbox-agent wires from a primary
// volume during boot. Path may contain the %HOME% token; UID and GID accept
// either a JSON number or a runtime token (%UID%/%GID%); Mode is an octal
// string (e.g. "0755"). All are resolved at mount time via ResolveVolumes.
type Volume struct {
	Path   string      `json:"path"`
	Volume VolumeKind  `json:"volume"`
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

// ResolvedVolume is a Volume with all tokens expanded and fields parsed.
type ResolvedVolume struct {
	Path string
	Kind VolumeKind
	UID  *int
	GID  *int
	Mode *os.FileMode
}

// ResolveVolumes expands every declared volume's tokens against the runtime
// identity and validates the result.
func ResolveVolumes(volumes []Volume, rt VolumeRuntime) ([]ResolvedVolume, error) {
	if len(volumes) == 0 {
		return nil, nil
	}
	out := make([]ResolvedVolume, 0, len(volumes))
	for idx, v := range volumes {
		path := expandVolumeToken(v.Path, rt)
		if strings.TrimSpace(path) == "" {
			return nil, fmt.Errorf("volume[%d]: path is required", idx)
		}
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("volume %q: path must be absolute", path)
		}
		switch v.Volume {
		case VolumeData, VolumeCache:
		default:
			return nil, fmt.Errorf("volume %q: unknown volume kind %q", path, v.Volume)
		}
		rv := ResolvedVolume{Path: filepath.Clean(path), Kind: v.Volume}
		if uid, ok, err := resolveScalar(v.UID, rt); err != nil {
			return nil, fmt.Errorf("volume %q uid: %w", path, err)
		} else if ok {
			rv.UID = &uid
		}
		if gid, ok, err := resolveScalar(v.GID, rt); err != nil {
			return nil, fmt.Errorf("volume %q gid: %w", path, err)
		} else if ok {
			rv.GID = &gid
		}
		if mode := strings.TrimSpace(v.Mode); mode != "" {
			parsed, err := strconv.ParseUint(mode, 8, 32)
			if err != nil {
				return nil, fmt.Errorf("volume %q mode %q: %w", path, mode, err)
			}
			m := os.FileMode(parsed)
			rv.Mode = &m
		}
		out = append(out, rv)
	}
	return out, nil
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
// gets for the same reason (ADR 0032 §5).
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
