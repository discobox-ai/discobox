package access

import (
	"fmt"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/agentcreds"
)

// --debug prints the call a command makes, both ways, on stderr, and leaves
// stdout to the result.
func TestDebugPrintsEachCallAndItsAnswer(t *testing.T) {
	serve(t, &fakeService{})

	stdout, stderr, code := capture(t, "", func() int {
		return Run([]string{"--debug", "request", "--name", "github", "--env-var", "GITHUB_TOKEN",
			"--hosts", "api.github.com", "--why", "open a PR", "--use", "Open a PR against the current repo"})
	})
	if code != exitOK {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	for _, want := range []string{
		"discobox-access: debug: POST http://127.0.0.1:",
		agentcreds.PathRequests,
		`debug: request body: {`,
		`"justification":"open a PR"`,
		"debug: 202 Accepted",
		`debug: response body: {`,
		`"requestId":"sreq_1"`,
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stdout, "debug:") {
		t.Fatalf("stdout = %q, want the debug lines on stderr only", stdout)
	}
}

// The value a use answers with is printed as redacted: --debug is not a way to
// take a value without a command (ADR 0092). The child still gets it.
func TestDebugRedactsTheValueAUseAnswersWith(t *testing.T) {
	serve(t, &fakeService{})

	stdout, stderr, code := capture(t, "", func() int {
		return Run([]string{"--debug", "run", "--use", "use_7f3c", "--", "sh", "-c", "printf %s \"$GITHUB_TOKEN\""})
	})
	if code != exitOK {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if stdout != "ghp_stand_in" {
		t.Fatalf("child saw %q, want the value", stdout)
	}
	if strings.Contains(stderr, "ghp_stand_in") {
		t.Fatalf("stderr carries the value:\n%s", stderr)
	}
	for _, want := range []string{
		agentcreds.PathUse,
		`"useId":"use_7f3c"`,
		`"value":"<redacted>"`,
		`"envVar":"GITHUB_TOKEN"`,
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// A refusal is printed as it was answered, which is what --debug is for.
func TestDebugPrintsARefusal(t *testing.T) {
	serve(t, &fakeService{getErr: fmt.Errorf("%w: not the approved use", agentcreds.ErrDenied)})

	_, stderr, code := capture(t, "", func() int {
		return Run([]string{"--debug", "run", "--use", "use_7f3c", "--", "true"})
	})
	if code != exitError || !strings.Contains(stderr, "debug: 403") || !strings.Contains(stderr, "not the approved use") {
		t.Fatalf("exit = %d, stderr:\n%s\nwant the refusal's status and body", code, stderr)
	}
}

// A use answer that is not a JSON object cannot be told apart from a value,
// so none of it is shown.
func TestDebugShowsNothingOfAUseAnswerItCannotRead(t *testing.T) {
	if got := redactValue(agentcreds.PathUse, []byte("ghp_raw")); got != redacted {
		t.Fatalf("redactValue() = %q, want it all redacted", got)
	}
	if got := redactValue(agentcreds.PathCredentials, []byte("not json")); got != "not json" {
		t.Fatalf("redactValue() = %q, want another call's body as it was", got)
	}
}

// Without --debug nothing is printed about the calls.
func TestWithoutDebugNothingIsPrinted(t *testing.T) {
	serve(t, &fakeService{})
	_, stderr, code := capture(t, "", func() int { return Run([]string{"list"}) })
	if code != exitOK || strings.Contains(stderr, "debug:") {
		t.Fatalf("exit = %d, stderr %q, want no debug lines", code, stderr)
	}
}
