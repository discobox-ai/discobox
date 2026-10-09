// Package clientconfig loads the discobox CLI's own configuration file,
// client.yaml: what this client does when a command line leaves a choice
// unsaid (ADR 26-10-09-389). It is read the way the server reads server.yaml
// (ADR 0096) — Config's struct tags are the source of truth, a key nothing
// defines is an error naming it, and a reference listing every setting is
// generated from the same tags.
package clientconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/adrg/xdg"

	"github.com/discobox-ai/discobox/configfile"
)

// FileVar names the configuration file, and DefaultFileName is what Load reads
// inside the platform's config directory when it does not. Set empty, FileVar
// reads no file.
//
// It is not DISCOBOX_CONFIG_FILE: that one names the server's file, and
// `discobox server --config-file` sets it for the server it runs.
const (
	FileVar         = "DISCOBOX_CLIENT_CONFIG_FILE"
	DefaultFileName = "client.yaml"
	// ExampleFileName is the reference written beside the file when there is
	// none.
	ExampleFileName = "client.example.yaml"
)

// SchemaID is where the generated schema is published, and what a
// `# yaml-language-server: $schema=` modeline points at.
const SchemaID = "https://raw.githubusercontent.com/discobox-ai/discobox/main/cli/config.schema.json"

// Config is the whole file. Its tags are what configfile reads: `yaml` the key,
// `doc` the description, `default` the literal default and `example` a sample
// value. A command-line flag always wins over the setting it defaults.
type Config struct {
	New New `yaml:"new" doc:"What discobox new, and the console's New Discobox panel, take when the command line does not say."`

	// Path is the file read, or empty when FileVar says read none. Read is
	// whether one was there to read.
	Path string `yaml:"-"`
	Read bool   `yaml:"-"`
}

// New is what a create takes by default.
type New struct {
	Skills     []string `yaml:"skills" doc:"Directories of skills to install in every new discobox, each a subdirectory holding a SKILL.md, as --skills reads them. A relative path is relative to this file's directory, and ~ is your home directory. --skills on the command line replaces this list rather than adding to it." example:"[\"~/team-skills\"]"`
	UserSkills bool     `yaml:"userSkills" doc:"Install your own skills from ~/.claude/skills and ~/.agents/skills in every new discobox, as --user-skills does. --user-skills=false on the command line turns it off for one discobox."`
}

// Path is the file Load reads: FileVar when set, otherwise DefaultFileName
// under the platform's config directory, beside the server's server.yaml. An
// empty FileVar means read nothing.
func Path() string {
	if name, ok := os.LookupEnv(FileVar); ok {
		return strings.TrimSpace(name)
	}
	return filepath.Join(xdg.ConfigHome, "discobox", DefaultFileName)
}

// Load reads the configuration file.
//
// A missing file at the default path is not an error: a client with no file
// behaves exactly as one always has. A path FileVar names that does not exist
// is, as is a file that does not parse or names a key nothing defines.
func Load() (*Config, error) {
	cfg := &Config{}
	if err := configfile.ApplyDefaults(cfg); err != nil {
		return nil, err
	}
	path := Path()
	cfg.Path = path
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if _, err := configfile.Decode(cfg, data, path); err != nil {
			return nil, err
		}
		cfg.Read = true
	case os.IsNotExist(err):
		if _, named := os.LookupEnv(FileVar); named {
			return nil, fmt.Errorf("%s names %s, which does not exist", FileVar, path)
		}
		return cfg, nil
	default:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := cfg.resolvePaths(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// resolvePaths makes the file's directories absolute: ~ against the home
// directory, and a relative path against the file's own directory, which is
// where whoever wrote it was looking — not wherever the command later runs.
func (cfg *Config) resolvePaths() error {
	base := filepath.Dir(cfg.Path)
	for i, dir := range cfg.New.Skills {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			return fmt.Errorf("%s: new.skills[%d] is empty; name a directory", cfg.Path, i)
		}
		if dir == "~" || strings.HasPrefix(dir, "~/") || strings.HasPrefix(dir, "~"+string(filepath.Separator)) {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("%s: new.skills[%d]: %w", cfg.Path, i, err)
			}
			dir = filepath.Join(home, dir[1:])
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(base, dir)
		}
		cfg.New.Skills[i] = filepath.Clean(dir)
	}
	return nil
}

// exampleHeader introduces the generated reference file.
const exampleHeader = `# yaml-language-server: $schema=` + SchemaID + `
#
# discobox client configuration reference.
#
# Every setting is listed here, commented out, showing the value it has when
# nothing sets it. Copy this to the path the client reads —
# <XDG config home>/discobox/client.yaml, or wherever
# DISCOBOX_CLIENT_CONFIG_FILE names — and uncomment only what you want to
# change: deleting the "#" from a setting's line leaves correctly indented
# YAML, at any depth. A nested setting needs its parent uncommented too. A
# client with no file at all behaves exactly as one always has.
#
# A client that looks for this file and finds none rewrites this reference
# beside where that file belongs, so it always matches the client that wrote
# it. Copy it rather than editing it in place.
#
# A flag given on the command line wins over the setting it defaults. A key
# this client does not define is an error naming the key.
#
# This file is generated from the tags on clientconfig.Config by
# ` + "`go tool task generate`" + `. Edit the struct, not this.
`

// ExampleYAML renders the commented reference form of the configuration file.
func ExampleYAML() ([]byte, error) {
	return configfile.Example(exampleHeader, reflect.TypeOf(Config{}))
}

// Schema builds the JSON Schema for the configuration file from the tags on
// Config.
func Schema() (map[string]any, error) {
	return configfile.Schema(reflect.TypeOf(Config{}), SchemaID,
		"Discobox client configuration",
		"Configuration for the discobox CLI: what its commands take when the command line does not say. A flag given on the command line wins over the setting it defaults.")
}

// RefreshExample writes the reference file beside configFile, the path the
// client looked for its configuration at and found nothing, and returns where
// it wrote it. It is rewritten on every such look rather than written once,
// because a reference left by an older client is missing whatever was added
// since; an unchanged one is left alone.
func RefreshExample(configFile string) (string, error) {
	body, err := ExampleYAML()
	if err != nil {
		return "", err
	}
	path := filepath.Join(filepath.Dir(configFile), ExampleFileName)
	if err := configfile.WriteExample(path, body); err != nil {
		return "", err
	}
	return path, nil
}
