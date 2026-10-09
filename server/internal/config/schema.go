package config

import (
	"reflect"

	"github.com/discobox-ai/discobox/configfile"
)

// SchemaID is where the generated schema is published, and what a
// `# yaml-language-server: $schema=` modeline points at.
const SchemaID = "https://raw.githubusercontent.com/discobox-ai/discobox/main/server/config.schema.json"

// Schema builds the JSON Schema for the configuration file from the tags on
// Config (ADR 0096 §2, configuration file).
func Schema() (map[string]any, error) {
	return configfile.Schema(reflect.TypeOf(Config{}), SchemaID,
		"Discobox server configuration",
		"Configuration for discobox-server. Every setting may also be set by the environment variable named in its description, which takes precedence over this file.")
}

// SettingPaths lists every configurable path, sorted. It exists for tests that
// assert the file, the schema and the loader describe the same surface.
func SettingPaths() []string {
	return configfile.SettingPaths(reflect.TypeOf(Config{}))
}

// SettingEnv returns the environment variable that overrides a path, and
// whether it has one.
func SettingEnv(path string) (string, bool) {
	for _, s := range configfile.Settings(reflect.TypeOf(Config{})) {
		if s.Path == path {
			return s.Env, s.Env != ""
		}
	}
	return "", false
}
