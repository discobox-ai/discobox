package cli

import (
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// Hook payloads are composed inside the discobox too, and one that is not valid
// JSON is printed as its raw bytes.
func TestHarnessHookLogsEscapeTheirPayload(t *testing.T) {
	out := new(strings.Builder)
	if err := harnessHookTable(false).write(out, []apimodel.HarnessHookLog{{
		// Every column but the time comes from a database the discobox can
		// rewrite, so each one carries a hostile rune here.
		TerminalId: apiclientgen.NewOptString("term\x1b[1A"),
		Provider:   "claude-code\u009b2K",
		Event:      "PreToolUse\u202e",
		Payload:    []byte("not json \x1b[2J\u202e"),
	}}, true, false); err != nil {
		t.Fatalf("write hook logs: %v", err)
	}
	for _, raw := range []string{"\x1b", "\u202e", "\u009b"} {
		if strings.Contains(out.String(), raw) {
			t.Fatalf("hook log carries raw %q:\n%s", raw, out.String())
		}
	}
}
