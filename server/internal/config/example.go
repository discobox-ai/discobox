package config

import (
	"path/filepath"
	"reflect"

	"github.com/discobox-ai/discobox/configfile"
)

// ExampleFileName is the reference a server writes beside the configuration
// file it looked for and did not find.
const ExampleFileName = "server.example.yaml"

// exampleHeader introduces the generated reference file.
const exampleHeader = `# yaml-language-server: $schema=` + SchemaID + `
#
# discobox-server configuration reference.
#
# Every setting is listed here, commented out, showing the value it has when
# nothing sets it. Copy this to the path the server reads —
# <XDG config home>/discobox/server.yaml, or wherever DISCOBOX_CONFIG_FILE
# names — and uncomment only what you want to change: deleting the "#" from a
# setting's line leaves correctly indented YAML, at any depth. A nested setting
# needs its parent uncommented too, which is the one place a single deletion is
# not enough. A server with no file at all behaves exactly as one always has.
#
# A server that starts and finds no configuration file rewrites this reference
# beside where that file belongs, so it always matches the server that wrote
# it. Copy it rather than editing it in place.
#
# Every setting can also be set by the environment variable named beside it,
# and the environment wins over this file.
#
# A key this server does not define is a startup failure naming the key. That
# is the point of the file: a misspelled environment variable is silently the
# default instead.
#
# This file is generated from the tags on config.Config by
# ` + "`go tool task generate`" + `. Edit the struct, not this.
`

// ExampleYAML renders the commented reference form of the configuration file.
//
// It is generated rather than written because a hand-kept reference is a
// reference that is missing the setting somebody added last week, and the
// whole claim of ADR 0096 (configuration file) is that the file holds all valid configuration.
func ExampleYAML() ([]byte, error) {
	return configfile.Example(exampleHeader, reflect.TypeOf(Config{}))
}

// RefreshExample writes the reference file beside configFile, the path a
// server looked for its configuration at and found nothing, and returns where it
// wrote it.
//
// It is rewritten on every such start rather than written once, because a
// reference left by an older server is missing whatever was added since — the
// same drift ExampleYAML is generated to avoid. It is never written beside a
// file that exists: an operator who has one reads their own. An unchanged
// reference is left untouched, and a changed one is replaced by rename, so two
// servers starting at once cannot leave half a file.
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
