package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// `service ls` prints what the box wrote — a declaration's name, description
// and problem, and the IDs the sandbox answered with — so in every output
// form a control sequence in any of it is shown, not obeyed.
func TestServiceListingIsTerminalSafe(t *testing.T) {
	t.Parallel()
	services := []apimodel.SandboxService{
		{ID: hostile, Name: hostile, Status: apiclientgen.SandboxServiceStatus(hostile), Description: apiclientgen.NewOptString(hostile)},
		{ID: "broken", Name: hostile, Status: apiclientgen.SandboxServiceStatusStopped, Problem: apiclientgen.NewOptString(hostile)},
	}
	run := func(app *App) string {
		t.Helper()
		var stdout bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&stdout)
		cmd.SetErr(&bytes.Buffer{})
		if err := app.writeSandboxServices(cmd, services, time.Time{}); err != nil {
			t.Fatalf("write: %v", err)
		}
		return stdout.String()
	}
	for name, out := range map[string]string{
		"table": run(&App{}),
		"quiet": run(&App{quiet: true}),
		"json":  run(&App{output: "json"}),
	} {
		for _, raw := range []string{"\x1b", "\x07", "\u202e"} {
			if strings.Contains(out, raw) {
				t.Errorf("%s: %q reached the terminal raw:\n%s", name, raw, out)
			}
		}
	}
	table := run(&App{})
	if !strings.Contains(table, hostileShown) || !strings.Contains(table, "cannot run: "+hostileShown) {
		t.Errorf("table should show the name and problem escaped:\n%s", table)
	}
}
