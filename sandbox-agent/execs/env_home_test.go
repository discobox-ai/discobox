package execs

import (
	"os/user"
	"strings"
	"testing"
)

// A sandbox whose manifest names nobody still runs as somebody. Saying nothing
// about who to run as (ADR 0025 §5) does not mean knowing nothing about where
// home is: the exec inherits this process's identity, whose account has one.
//
// Every sandbox the server creates for itself lands here — a configure sandbox
// carries no user at all — and without this its PATH keeps a literal %HOME%
// and its harness has no home to install files into.
func TestEnvDefaultsFindHomeWhenNobodyIsNamed(t *testing.T) {
	current, err := user.Current()
	if err != nil || strings.TrimSpace(current.HomeDir) == "" {
		t.Skip("this process has no resolvable home")
	}

	env := EnvWithRuntimeDefaults(map[string]string{"PATH": "%HOME%/.npm-global/bin:/usr/bin"}, nil)

	if got := env["HOME"]; got != current.HomeDir {
		t.Fatalf("HOME = %q, want this process's own home %q", got, current.HomeDir)
	}
	if strings.Contains(env["PATH"], "%HOME%") {
		t.Fatalf("PATH still carries an unexpanded home token: %q", env["PATH"])
	}
}

// What the env already says wins: the sandbox's own HOME is not second-guessed.
func TestEnvDefaultsKeepAnExplicitHome(t *testing.T) {
	env := EnvWithRuntimeDefaults(map[string]string{"HOME": "/explicit"}, nil)
	if got := env["HOME"]; got != "/explicit" {
		t.Fatalf("HOME = %q, want the value the env carried", got)
	}
}
