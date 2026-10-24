package cli

import (
	"github.com/discobox-ai/discobox/cli/internal/clientconfig"
)

// loadClientConfig is this client's own configuration file, client.yaml
// (ADR 26-10-09-389), read again on every call rather than once: the console
// is one long-lived process, and a person who fixes or edits the file between
// two creates from it means the second to see what they wrote.
func loadClientConfig() (*clientconfig.Config, error) {
	cfg, err := clientconfig.Load()
	if err != nil {
		return nil, err
	}
	refreshClientConfigExample()
	return cfg, nil
}

// refreshClientConfigExample rewrites client.example.yaml, the reference
// listing every setting, when the client reads the default path and there is
// no client.yaml there (clientconfig.MissingDefault) — the way the server
// rewrites server.example.yaml. Reading the configuration calls it, and so
// does the console as it starts, so the reference is there to find before
// anything is created. It is documentation, so one that cannot be written
// fails nothing.
func refreshClientConfigExample() {
	if path, ok := clientconfig.MissingDefault(); ok {
		_, _ = clientconfig.RefreshExample(path)
	}
}

// newSkills is what a new discobox installs as skills: --skills and
// --user-skills as given, and for either one that was not — nil — what
// client.yaml's new section says. A flag given replaces the setting rather
// than adding to it, so a command line always says exactly what it means.
//
// The file is read even when both are given: one that does not parse stops
// every create, not only the ones that happen to need a setting from it.
func newSkills(dirs []string, user *bool) ([]string, bool, error) {
	cfg, err := loadClientConfig()
	if err != nil {
		return nil, false, err
	}
	if dirs == nil {
		dirs = cfg.Run.Skills
	}
	userSkills := cfg.Run.UserSkills
	if user != nil {
		userSkills = *user
	}
	return dirs, userSkills, nil
}
