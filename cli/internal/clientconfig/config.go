// Package clientconfig loads the discobox CLI's own configuration file,
// client.yaml: what this client does when a command line leaves a choice
// unsaid (ADR 26-10-09-389). It is read the way the server reads server.yaml
// (ADR 0096) — Config's struct tags are the source of truth, a key nothing
// defines is an error naming it, and a reference listing every setting is
// generated from the same tags.
package clientconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/adrg/xdg"

	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/discobox-ai/discobox/pkg/configfile"
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
	// ExampleFileName is the reference written beside the default path when
	// no client.yaml is there (MissingDefault).
	ExampleFileName = "client.example.yaml"
)

// SchemaID is where the generated schema is published, and what a
// `# yaml-language-server: $schema=` modeline points at.
const SchemaID = "https://raw.githubusercontent.com/discobox-ai/discobox/main/cli/config.schema.json"

// Config is the whole file. Its tags are what configfile reads: `yaml` the key,
// `doc` the description, `default` the literal default and `example` a sample
// value. A command-line flag always wins over the setting it defaults.
type Config struct {
	New New `yaml:"new" doc:"What discobox run, and the console's New Discobox panel, take when the command line does not say."`

	// Path is the file read, or empty when FileVar says read none. Read is
	// whether one was there to read.
	Path string `yaml:"-"`
	Read bool   `yaml:"-"`
}

// New is what a create takes by default.
type New struct {
	Skills     []string `yaml:"skills" doc:"Skills to install in every new discobox, as --skills reads them: a directory whose subdirectories holding a SKILL.md are each a skill, or a Git URL[@REF][//PATH] whose skills are linked from a checkout of it. A relative path is relative to this file's directory, and ~ is your home directory. --skills on the command line replaces this list rather than adding to it. The project's skills repositories (discobox skills) are added ahead of these either way. discobox skills add DIR and rm DIR edit this list in place." example:"[\"~/team-skills\"]"`
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
	for i, entry := range cfg.New.Skills {
		resolved, err := ResolveSkill(base, entry)
		if err != nil {
			return fmt.Errorf("%s: new.skills[%d]: %w", cfg.Path, i, err)
		}
		cfg.New.Skills[i] = resolved
	}
	return nil
}

// ResolveSkill is one skills entry as a create reads it, judged against base
// — the file's directory for an entry of new.skills, the working directory
// for one typed on a command line (ADR 26-10-15-664): a skills source with
// GitHub shorthand expanded, so a create reads it the same from any
// directory, and a directory made absolute, with ~ for the home directory.
func ResolveSkill(base, entry string) (string, error) {
	resolved, _, err := resolveSkill(base, entry)
	return resolved, err
}

// SkillKey tells one skills entry from another, as ResolveSkill reads both:
// every spelling of one repository, ref and directory is one key, and a
// directory is its absolute path.
func SkillKey(base, entry string) (string, error) {
	_, key, err := resolveSkill(base, entry)
	return key, err
}

func resolveSkill(base, entry string) (resolved, key string, err error) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", "", errors.New("is empty; name a directory or a Git URL")
	}
	repo, dir, err := sandboxcreate.ParseSkills(entry, base)
	switch {
	case err != nil:
		// Kept as written, so a create refuses it naming it, and every other
		// command — the one that removes it among them — still reads the file.
		return entry, entry, nil //nolint:nilerr // the refusal is the create's to make
	case dir != "":
		return dir, dir, nil
	}
	// A Git URL names a skills source, which is not on this machine.
	return repo.Value(), repo.Key(), nil
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
# A client that reads the default path and finds no client.yaml there rewrites
# this reference beside it, so it always matches the client that wrote it.
# Nothing is written beside a path DISCOBOX_CLIENT_CONFIG_FILE names. Copy
# this file rather than editing it in place.
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

// MissingDefault is the default path and true when that is where the client
// reads its configuration and nothing is there: the one case the reference is
// written for. A path FileVar names is the user's own, anywhere — a missing
// one is Load's error, and nothing is written beside it.
func MissingDefault() (string, bool) {
	if _, named := os.LookupEnv(FileVar); named {
		return "", false
	}
	path := Path()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		return "", false
	}
	return path, true
}

// RefreshExample writes the reference file beside configFile — the default
// path, when no client.yaml is there, which is what MissingDefault returns —
// and returns where it wrote it. It is rewritten every time rather than
// written once, because a reference left by an older client is missing
// whatever was added since; an unchanged one is left alone.
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
