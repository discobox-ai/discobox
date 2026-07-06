package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const DefaultImageConfigPath = "/usr/share/discobox/image.json"

type ImageConfig struct {
	Env map[string]string `json:"env,omitempty"`
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
	return cfg, nil
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
