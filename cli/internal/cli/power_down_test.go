package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/cli/internal/tui"
)

func poweredDownSandbox(display apiclientgen.SandboxRuntimeDisplayState, reason apiclientgen.SandboxPowerDownReason) apiclientgen.Sandbox {
	var sb apiclientgen.Sandbox
	sb.Runtime.DisplayState = apiclientgen.NewOptSandboxRuntimeDisplayState(display)
	sb.Runtime.PowerDown = apiclientgen.NewOptSandboxPowerDown(apiclientgen.SandboxPowerDown{Reason: reason, At: time.Now()})
	return sb
}

// A discobox that powered itself down says why in the STATE a person reads,
// so an idle stop is not taken for a crash; one with no power-down is plainly
// stopped.
func TestTheStateSaysWhyADiscoboxPoweredItselfDown(t *testing.T) {
	for _, tc := range []struct {
		display apiclientgen.SandboxRuntimeDisplayState
		reason  apiclientgen.SandboxPowerDownReason
		want    string
	}{
		{apiclientgen.SandboxRuntimeDisplayStateStopped, apiclientgen.SandboxPowerDownReasonIdle, "stopped (idle)"},
		{apiclientgen.SandboxRuntimeDisplayStateSuspended, apiclientgen.SandboxPowerDownReasonSleep, "paused (asleep)"},
		{apiclientgen.SandboxRuntimeDisplayStateStopping, apiclientgen.SandboxPowerDownReasonExit, "stopping (shell exited)"},
		// A reason a newer server knows and this CLI does not is left unsaid.
		{apiclientgen.SandboxRuntimeDisplayStateStopped, "hibernate", "stopped"},
		// A power-down is never said of a box that is up again.
		{apiclientgen.SandboxRuntimeDisplayStateRunning, apiclientgen.SandboxPowerDownReasonIdle, "running"},
	} {
		if got := sandboxDisplayState(poweredDownSandbox(tc.display, tc.reason)); got != tc.want {
			t.Errorf("%s/%s reads %q, want %q", tc.display, tc.reason, got, tc.want)
		}
	}
	var stopped apiclientgen.Sandbox
	stopped.Runtime.DisplayState = apiclientgen.NewOptSandboxRuntimeDisplayState(apiclientgen.SandboxRuntimeDisplayStateStopped)
	if got := sandboxDisplayState(stopped); got != "stopped" {
		t.Errorf("a box with no power-down reads %q, want stopped", got)
	}
}

// An event of a discobox that powered itself down says why after its power,
// in the API's word as power= is.
func TestAnEventSaysWhyADiscoboxPoweredItselfDown(t *testing.T) {
	app := &App{}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	for _, data := range []sandboxEventData{
		{SandboxID: "sbx_a", State: "ready", RuntimeState: "stopped", PowerDownReason: "idle"},
		{SandboxID: "sbx_b", State: "ready", RuntimeState: "stopped"},
	} {
		if err := app.writeSandboxEvent(cmd, sandboxEvent{Event: "sandbox.updated", Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "power=stopped  why=idle") || strings.Contains(lines[1], "why=") {
		t.Fatalf("printed:\n%s\nwant why=idle, the API's word, on the idle stop alone", out.String())
	}
}

// The console's row carries why the box powered itself down.
func TestTheConsoleRowCarriesWhyADiscoboxPoweredItselfDown(t *testing.T) {
	row := toTUISandbox(poweredDownSandbox(apiclientgen.SandboxRuntimeDisplayStateStopped, apiclientgen.SandboxPowerDownReasonIdle), "")
	if row.State != tui.StateStopped || !strings.Contains(row.PowerDown, "idle") {
		t.Fatalf("row = %s %q, want stopped and why", row.State, row.PowerDown)
	}
}
