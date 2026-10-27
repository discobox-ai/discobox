package access

import (
	"fmt"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
)

// --debug prints the call a command makes, both ways, on stderr, and leaves
// stdout to the result.
func TestDebugPrintsEachCallAndItsAnswer(t *testing.T) {
	serve(t, &fakeService{})

	stdout, stderr, code := capture(t, "", func() int {
		return Run([]string{"--debug", "request", "--name", "github", "--env-var", "GITHUB_TOKEN",
			"--hosts", "api.github.com", "--justification", "open a PR"})
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

// A refusal is printed as it was answered, which is what --debug is for.
func TestDebugPrintsARefusal(t *testing.T) {
	serve(t, &fakeService{statusErr: fmt.Errorf("%w: not this sandbox's request", agentcreds.ErrDenied)})

	_, stderr, code := capture(t, "", func() int {
		return Run([]string{"--debug", "wait", "--timeout", "5s", "request", "sreq_9"})
	})
	if code != exitError || !strings.Contains(stderr, "debug: 403") || !strings.Contains(stderr, "not this sandbox's request") {
		t.Fatalf("exit = %d, stderr:\n%s\nwant the refusal's status and body", code, stderr)
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
