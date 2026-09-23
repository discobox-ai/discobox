package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/health"
)

// With no terminal to ask on, the CLI never answers for the user: it says why
// libkrun cannot run, what Docker would give up, and the command that chooses
// it — and the server is left waiting (ADR 0148 §3).
func TestNoTerminalLeavesTheDefaultProviderChoiceToTheUser(t *testing.T) {
	var errOut bytes.Buffer
	app := &App{errOut: &errOut}
	err := app.answerDefaultProviderChoice(t.Context(), health.Choice{
		Provider:     "libkrun",
		Reason:       health.ReasonKVMUnavailable,
		Detail:       "open /dev/kvm: no such file or directory",
		Alternatives: []string{"docker"},
	}, strings.NewReader("y\n"))
	if err == nil {
		t.Fatal("a CLI with no terminal chose a provider")
	}
	for _, want := range []string{
		"KVM is not available",
		"/dev/kvm",
		"no VM boundary",
		"discobox admin server choose-provider docker",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	if errOut.Len() != 0 {
		t.Fatalf("asked a question with nobody to answer it: %q", errOut.String())
	}
}
