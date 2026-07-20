package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/obot-platform/discobox/harness"
)

const (
	DefaultImageConfigPath = "/usr/share/discobox/image.json"
	ImageAPIVersion        = "discobox.dev/image/v1"
)

type ImageConfig struct {
	APIVersion string            `json:"apiVersion"`
	Env        map[string]string `json:"env,omitempty"`
	Volumes    []Volume          `json:"volumes,omitempty"`
	Harness    *harness.Image    `json:"harness,omitempty"`
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
	UID    scalarToken `json:"uid,omitempty"`
	GID    scalarToken `json:"gid,omitempty"`
	Mode   string      `json:"mode,omitempty"`
}

// scalarToken holds a JSON scalar that is either an integer literal or a token
// string. An empty value means the field was omitted.
type scalarToken string

func (s *scalarToken) UnmarshalJSON(b []byte) error {
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
		*s = scalarToken(str)
		return nil
	}
	*s = scalarToken(string(b))
	return nil
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
func (i ImageConfig) ResolveVolumes(rt VolumeRuntime) ([]ResolvedVolume, error) {
	if len(i.Volumes) == 0 {
		return nil, nil
	}
	out := make([]ResolvedVolume, 0, len(i.Volumes))
	for idx, v := range i.Volumes {
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

func resolveScalar(tok scalarToken, rt VolumeRuntime) (int, bool, error) {
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

func LoadImage(path string) (ImageConfig, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultImageConfigPath
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ImageConfig{}, nil
	}
	if err != nil {
		return ImageConfig{}, fmt.Errorf("read image config %s: %w", path, err)
	}
	var cfg ImageConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ImageConfig{}, fmt.Errorf("parse image config %s: %w", path, err)
	}
	if cfg.APIVersion != ImageAPIVersion {
		return ImageConfig{}, fmt.Errorf("image config apiVersion = %q, want %q", cfg.APIVersion, ImageAPIVersion)
	}
	return cfg, nil
}

// HarnessForMode returns the single harness baked into the image. Config mode
// replaces only its primary command; all identity, files, and hook behavior
// remain image-owned.
func (i ImageConfig) HarnessForMode(mode string) (Harness, bool, error) {
	if i.Harness == nil {
		return Harness{}, false, nil
	}
	command := cloneCommand(i.Harness.RunCommand)
	if strings.TrimSpace(mode) == "config" {
		if i.Harness.Config == nil || len(i.Harness.Config.Command) == 0 {
			return Harness{}, false, fmt.Errorf("harness image %q does not support config mode", i.Harness.ID)
		}
		command = cloneCommand(i.Harness.Config.Command)
	}
	files := make([]HarnessFile, 0, len(i.Harness.Files))
	for _, file := range i.Harness.Files {
		files = append(files, HarnessFile{
			Path: file.Path, Content: file.Content,
			CreateOnly: file.CreateOnly, Template: file.Template,
		})
	}
	return Harness{
		ID:              i.Harness.ID,
		TypeID:          i.Harness.ID,
		Name:            i.Harness.Name,
		Command:         command,
		RelaunchCommand: cloneCommand(i.Harness.RelaunchCommand),
		IsDefault:       true,
		Files:           files,
	}, true, nil
}

func ApplyImageEnvDefaults(env map[string]string, image ImageConfig) map[string]string {
	if env == nil {
		env = map[string]string{}
	}
	for key, value := range image.Env {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, ok := env[key]; ok {
			continue
		}
		env[key] = expandImageEnvValue(value, env)
	}
	return env
}

func expandImageEnvValue(value string, env map[string]string) string {
	home := env["HOME"]
	return strings.ReplaceAll(value, "%HOME%", home)
}
