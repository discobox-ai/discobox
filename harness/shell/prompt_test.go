package shell

import (
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// The shell harness's discobox-prompt answers the command judge without a
// model, in the verdict shape discobox-access reads: refused by default, as the
// judge fails closed, and allowed only when DISCOBOX_SHELL_JUDGE=allow. It
// answers no other role.
func TestShellPromptAnswersTheJudgeWithoutAModel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the harness image's scripts run on Linux")
	}
	judge := []string{"prompt.sh", "--model", "judge", "--system", "decide", "--prompt", "gh pr create",
		"--output-schema", `{"type":"object"}`, "--no-tools"}
	run := func(env string, args ...string) (verdict struct {
		Allow  bool   `json:"allow"`
		Reason string `json:"reason"`
	}, err error) {
		cmd := exec.CommandContext(t.Context(), "sh", args...) //nolint:gosec // The wrapper under test, with this test's arguments.
		cmd.Env = append(os.Environ(), "DISCOBOX_SHELL_JUDGE="+env)
		out, err := cmd.Output()
		if err != nil {
			return verdict, err
		}
		return verdict, json.Unmarshal(out, &verdict)
	}

	for _, tc := range []struct {
		env   string
		allow bool
	}{{"", false}, {"deny", false}, {"yes", false}, {"allow", true}} {
		verdict, err := run(tc.env, judge...)
		if err != nil || verdict.Allow != tc.allow || verdict.Reason == "" {
			t.Errorf("DISCOBOX_SHELL_JUDGE=%q: verdict %+v, %v; want allow=%v with a reason", tc.env, verdict, err, tc.allow)
		}
	}
	if _, err := run("", "prompt.sh", "--model", "fast", "--prompt", "hello"); err == nil {
		t.Error("a role other than judge was answered; a shell runs no model to ask")
	}
}
