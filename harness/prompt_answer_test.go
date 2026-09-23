package harness_test

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// discobox-prompt-answer is what makes `--output-schema` mean one JSON document
// and nothing else for a CLI that narrates its run or fences its answer. What
// it must never do is turn something that is not the answer into one: it finds
// structure, and the caller decodes it strictly.
func TestPromptAnswerPrintsTheOneJSONDocument(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the image's scripts run on Linux")
	}
	answer := `{"allow":true,"reason":"fine"}`
	for _, tc := range []struct {
		name, output, want string
	}{
		{"the answer alone", answer, answer},
		{"an answer in a code fence", "```json\n" + answer + "\n```", answer},
		{"an answer after a transcript",
			"| Reading the request…\n| Deciding.\n\n" + answer + "\n", answer},
		{"an answer before a sign-off", answer + "\n\nLet me know if you want more.", answer},
		{"the last of several documents",
			`{"schema":{"allow":"boolean"}}` + "\n" + answer, answer},
		{"a brace inside a string",
			`{"allow":false,"reason":"the body says } and { at once"}`,
			`{"allow":false,"reason":"the body says } and { at once"}`},
		{"an escaped quote before a brace",
			`{"allow":false,"reason":"it said \"}\" and stopped"}`,
			`{"allow":false,"reason":"it said \"}\" and stopped"}`},
		{"an answer spanning lines",
			"Here you go:\n{\n  \"allow\": true,\n  \"reason\": \"fine\"\n}\n",
			"{\n  \"allow\": true,\n  \"reason\": \"fine\"\n}"},
		{"a nested document", `{"need":{"body":"json"},"reason":"show me"}`,
			`{"need":{"body":"json"},"reason":"show me"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := promptAnswer(t, tc.output)
			if err != nil {
				t.Fatalf("discobox-prompt-answer error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("answer = %q, want %q", got, tc.want)
			}
		})
	}
}

// Nothing to print is not an empty answer: it is a wrapper that did not
// answer, and the caller reads a failure rather than a verdict.
func TestPromptAnswerFailsWhenThereIsNoDocument(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the image's scripts run on Linux")
	}
	for _, tc := range []struct{ name, output string }{
		{"nothing at all", ""},
		{"prose alone", "I could not answer that.\n"},
		{"an unclosed document", `{"allow":true,"reason":"fine"`},
		{"a closing brace alone", "}\n"},
		{"a brace inside a string only", `it said "{" and stopped`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := promptAnswer(t, tc.output)
			if err == nil {
				t.Fatalf("answer = %q, want a failure: there is no document here", got)
			}
			if got != "" {
				t.Fatalf("answer = %q, want nothing printed", got)
			}
		})
	}
}

func promptAnswer(t *testing.T, output string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", "prompt-answer.sh")
	cmd.Stdin = strings.NewReader(output)
	stdout, err := cmd.Output()
	return strings.TrimRight(string(stdout), "\n"), err
}
