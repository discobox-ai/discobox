package cli

import (
	"strings"
	"testing"
)

func TestRunDashPrompt(t *testing.T) {
	setHome(t, t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(newRunSourceTestRepo(t))
	const multiline = "Fix the 'quotes' and $variables.\n\nKeep `code` and \\\"escaping\\\" intact.\n"
	for _, tc := range []struct {
		name, input string
		args        []string
		want        []any
		canceled    bool
	}{
		{name: "source", input: multiline, args: []string{"-d", ".", "-"}, want: []any{multiline}},
		{name: "no source", input: multiline, args: []string{"-d", "--no-source", "-"}, want: []any{multiline}},
		{name: "prompt flag", input: multiline, args: []string{"-d", ".", "-p", "-"}, want: []any{multiline}},
		{name: "empty", args: []string{"-d", "--no-source", "-"}, canceled: true},
		{name: "whitespace", input: " \n\t\n", args: []string{"-d", "--no-source", "-"}, canceled: true},
		{name: "dash among words", input: multiline, args: []string{"-d", "--no-source", "use", "-", "literally"}, want: []any{"use", "-", "literally"}},
		{name: "JSON stays literal", input: `{"noSource":true,"prompt":"-"}`, args: []string{"--from-json"}, want: []any{"-"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posted map[string]any
			server := runJSONServer(t, &posted)
			errOut, err := runWith(t, server.URL, tc.input, tc.args...)
			if tc.canceled {
				if err == nil || !strings.Contains(err.Error(), "canceled") || posted != nil {
					t.Fatalf("empty prompt: err=%v, posted=%v; want cancellation without creation", err, posted)
				}
				return
			}
			if err != nil {
				t.Fatalf("run: %v\n%s", err, errOut)
			}
			prompt := posted["config"].(map[string]any)["prompt"].([]any)
			if len(prompt) != len(tc.want) {
				t.Fatalf("prompt = %#v, want %#v", prompt, tc.want)
			}
			for i := range tc.want {
				if prompt[i] != tc.want[i] {
					t.Fatalf("prompt = %#v, want %#v", prompt, tc.want)
				}
			}
		})
	}
}
