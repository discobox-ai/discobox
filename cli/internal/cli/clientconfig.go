package cli

import (
	"github.com/discobox-ai/discobox/cli/internal/clientconfig"
)

// loadClientConfig is this client's own configuration file, client.yaml
// (ADR 26-10-09-389), read again on every call rather than once: the console
// is one long-lived process, and a person who fixes or edits the file between
// two creates from it means the second to see what they wrote.
//
// When there is no file where it was looked for, the reference listing every
// setting is rewritten beside where it belongs, the way the server rewrites
// server.example.yaml. That reference is documentation, so one that cannot be
// written is no reason to fail the command that looked.
func loadClientConfig() (*clientconfig.Config, error) {
	cfg, err := clientconfig.Load()
	if err != nil {
		return nil, err
	}
	if cfg.Path != "" && !cfg.Read {
		_, _ = clientconfig.RefreshExample(cfg.Path)
	}
	return cfg, nil
}

// newSkills is what a new discobox installs as skills: --skills and
// --user-skills as given, and for either one that was not — nil — what
// client.yaml's new section says. A flag given replaces the setting rather
// than adding to it, so a command line always says exactly what it means.
func newSkills(dirs []string, user *bool) ([]string, bool, error) {
	if dirs != nil && user != nil {
		return dirs, *user, nil
	}
	cfg, err := loadClientConfig()
	if err != nil {
		return nil, false, err
	}
	if dirs == nil {
		dirs = cfg.New.Skills
	}
	userSkills := cfg.New.UserSkills
	if user != nil {
		userSkills = *user
	}
	return dirs, userSkills, nil
}
