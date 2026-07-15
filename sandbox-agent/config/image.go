package config

import (
	"encoding/json"
	"fmt"
	"os"
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
	Harness    *harness.Image    `json:"harness,omitempty"`
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
