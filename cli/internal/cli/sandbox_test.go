package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

func TestCreateSandboxBodyIncludesHarnessLaunchFields(t *testing.T) {
	t.Setenv("SANDBOX_ENV_FROM_SHELL", "from-shell")
	body, err := createSandboxBody(context.Background(), sandboxCreateOptions{
		name:                 "work",
		harnessName:          "Codex",
		model:                "gpt-5.1-codex-max",
		modelServiceTier:     "priority",
		modelReasoningLevel:  "high",
		prompt:               []string{"implement this"},
		env:                  []string{"EXPLICIT=value", "SANDBOX_ENV_FROM_SHELL"},
		sourceURL:            "https://example.com/repo.git",
		sourceRef:            "main",
		sourceRefType:        "branch",
		sourceDirectory:      "/workspace/repo",
		workingDirectory:     "/workspace/repo",
		sourceCodeReferences: `{"lib":{"kind":"git","url":"https://example.com/lib.git","checkout":{"commit":"abc123","refType":"commit"},"destination":{"directory":"/workspace/lib"}}}`,
		userName:             "darren",
		userUID:              1000,
		userGroups:           []string{"1000", "docker"},
		// 0 is a meaningful uid, so the body only carries these when the flag
		// was actually given; the command sets these from Flags().Changed.
		userUIDSet:    true,
		homeDirectory: "/home/darren",
	})
	if err != nil {
		t.Fatalf("createSandboxBody: %v", err)
	}
	if body.HarnessName.Value != "Codex" || body.Config.Model.Value != "gpt-5.1-codex-max" ||
		len(body.Config.Prompt) != 1 || body.Config.Prompt[0] != "implement this" {
		t.Fatalf("harness fields = %#v", body)
	}
	env, ok := body.Config.Env.Get()
	if !ok {
		t.Fatal("expected env")
	}
	if env["EXPLICIT"] != "value" || env["SANDBOX_ENV_FROM_SHELL"] != "from-shell" {
		t.Fatalf("env = %#v, want explicit and shell values", env)
	}
	source, ok := body.Config.Source.Get()
	if !ok {
		t.Fatal("expected source")
	}
	if source.Kind != "git" {
		t.Fatalf("source kind = %q, want git", source.Kind)
	}
	destination, ok := source.Destination.Get()
	if !ok {
		t.Fatal("expected source destination")
	}
	if destination.Directory.Value != "/workspace/repo" || destination.WorkingDirectory.Value != "/workspace/repo" {
		t.Fatalf("directories = source %q working %q", destination.Directory.Value, destination.WorkingDirectory.Value)
	}
	sandboxUser, ok := body.Config.User.Get()
	if !ok {
		t.Fatal("expected user")
	}
	if sandboxUser.Name.Value != "darren" || sandboxUser.UID.Value != 1000 || sandboxUser.Gid.Value != 1000 || sandboxUser.HomeDirectory.Value != "/home/darren" {
		t.Fatalf("user = %s %d/%d home %s, want darren 1000/1000 /home/darren", sandboxUser.Name.Value, sandboxUser.UID.Value, sandboxUser.Gid.Value, sandboxUser.HomeDirectory.Value)
	}
	ref, ok := body.Config.SourceCodeReferences.Value["lib"]
	if !ok {
		t.Fatal("expected lib source reference")
	}
	refDestination, ok := ref.Destination.Get()
	if !ok {
		t.Fatal("expected lib destination")
	}
	if refDestination.Directory.Value != "/workspace/lib" {
		t.Fatalf("ref directory = %q, want /workspace/lib", refDestination.Directory.Value)
	}
}

func TestUpdateSandboxBodyIsNameOnly(t *testing.T) {
	cmd := &cobra.Command{}
	addUpdateFlags(cmd, &sandboxUpdateOptions{})
	if cmd.Flags().Lookup("description") != nil {
		t.Fatal("sandbox update should not expose description flag")
	}
	if cmd.Flags().Lookup("source-url") != nil {
		t.Fatal("sandbox update should not expose source-url flag")
	}
	if cmd.Flags().Lookup("name") == nil {
		t.Fatal("sandbox update should expose name flag")
	}
}

func TestTerminalDetachSequenceFilter(t *testing.T) {
	const ctrlA = 0x01
	f := newDetachFilter("ctrl+a")

	out, detach := f.filter([]byte("abc"))
	if detach || string(out) != "abc" || f.armed {
		t.Fatalf("plain input = %q detach=%t armed=%t", out, detach, f.armed)
	}

	// The leader is held back across reads: the chord is two keystrokes and
	// nothing says they arrive together.
	out, detach = f.filter([]byte{ctrlA})
	if detach || string(out) != "" || !f.armed {
		t.Fatalf("leader alone = %q detach=%t armed=%t", out, detach, f.armed)
	}

	// A leader that qualified nothing costs nothing: it is delivered along with
	// the key that followed it.
	out, detach = f.filter([]byte("x"))
	if detach || string(out) != string([]byte{ctrlA, 'x'}) || f.armed {
		t.Fatalf("non-detach followup = %v detach=%t armed=%t", out, detach, f.armed)
	}

	// Both forms of the second key detach, because Ctrl is usually still down.
	for _, second := range []byte{'d', 0x04} {
		f = newDetachFilter("ctrl+a")
		out, detach = f.filter([]byte{'h', 'i', ctrlA, second, 'j'})
		if !detach || string(out) != "hi" || f.armed {
			t.Fatalf("detach chord with %#x = %q detach=%t armed=%t", second, out, detach, f.armed)
		}
	}

	// The leader twice is how you type one.
	f = newDetachFilter("ctrl+a")
	out, detach = f.filter([]byte{ctrlA, ctrlA, 'z'})
	if detach || string(out) != string([]byte{ctrlA, 'z'}) || f.armed {
		t.Fatalf("leader twice = %v detach=%t armed=%t", out, detach, f.armed)
	}

	// The leader is the configured one, so under another leader Ctrl-A is just
	// a keystroke and d after it is just a d.
	f = newDetachFilter("ctrl+b")
	out, detach = f.filter([]byte{ctrlA, 'd'})
	if detach || string(out) != string([]byte{ctrlA, 'd'}) || f.armed {
		t.Fatalf("old chord under ctrl+b = %v detach=%t armed=%t", out, detach, f.armed)
	}
	out, detach = f.filter([]byte{0x02, 'd'})
	if !detach || len(out) != 0 || f.armed {
		t.Fatalf("ctrl+b chord = %v detach=%t armed=%t", out, detach, f.armed)
	}
}

// The hint the attach prints names the chord it actually installed.
func TestTerminalDetachHintFollowsTheLeader(t *testing.T) {
	if got := (&App{}).detachHint(); got != "Ctrl-A d" {
		t.Errorf("default detach hint = %q, want %q", got, "Ctrl-A d")
	}
	if got := (&App{leaderKey: "ctrl+b"}).detachHint(); got != "Ctrl-B d" {
		t.Errorf("ctrl+b detach hint = %q, want %q", got, "Ctrl-B d")
	}
}

// 0 is a real uid (root), so an omitted flag and an explicit --user-uid 0 must
// reach the server differently. Gating on `> 0` collapsed them and made
// explicit root impossible to request.
func TestSandboxUserDistinguishesUnsetFromExplicitZero(t *testing.T) {
	unset, _ := sandboxUserFromCreateOptions(sandboxCreateOptions{userName: "dev"})
	if unset.UID.Set {
		t.Fatal("an unset --user-uid must not be sent as 0")
	}

	explicit, ok := sandboxUserFromCreateOptions(sandboxCreateOptions{
		userName: "root", userUID: 0, userUIDSet: true,
	})
	if !ok || !explicit.UID.Set || explicit.UID.Value != 0 {
		t.Fatalf("explicit --user-uid 0 must be sent: set=%v value=%d", explicit.UID.Set, explicit.UID.Value)
	}
}

// A discobox whose image its pool cannot obtain will fail the same way however
// often it is retried, so the failure says what to do: upgrade when its harness
// has a newer image, and that there is none when it has not.
func TestSandboxFailureReasonSaysWhatToDoAboutAnUnavailableImage(t *testing.T) {
	failed := func(reason apiclientgen.OptSandboxRuntimeErrorReason, upgrade bool) *apimodel.Sandbox {
		sb := &apimodel.Sandbox{ID: "sbx_1"}
		sb.Runtime.State = apiclientgen.SandboxRuntimeStateFailed
		sb.Runtime.ErrorMessage = apiclientgen.NewOptString(`"harness:local" is not on this pool`)
		sb.Runtime.ErrorReason = reason
		if upgrade {
			sb.Runtime.Upgrade = apiclientgen.NewOptSandboxUpgrade(apimodel.SandboxUpgrade{Available: true})
		}
		return sb
	}
	unavailable := apiclientgen.NewOptSandboxRuntimeErrorReason(apiclientgen.SandboxRuntimeErrorReasonImageUnavailable)

	if got := sandboxFailureReason(failed(unavailable, true)); !strings.Contains(got, "discobox admin box upgrade sbx_1") {
		t.Fatalf("reason = %q, want the upgrade command", got)
	}
	if got := sandboxFailureReason(failed(unavailable, false)); !strings.Contains(got, "no newer image") || strings.Contains(got, "upgrade sbx_1") {
		t.Fatalf("reason = %q, want it to say there is nothing to upgrade to", got)
	}
	if got := sandboxFailureReason(failed(apiclientgen.OptSandboxRuntimeErrorReason{}, true)); got != `"harness:local" is not on this pool` {
		t.Fatalf("an unclassified failure = %q, want the message alone", got)
	}
}

// A failure that is an automatic repair's own is the end of what happens by
// itself — there is one per failure (ADR 26-10-12-711 §4) — so it carries the
// failure the repair was answering and the command that tries again. A failure
// with no such repair behind it, and a message that already names the command,
// are left alone.
func TestSandboxFailureReasonSendsAFailedAutomaticRepairToRepair(t *testing.T) {
	failed := func(message string, generation, repairGeneration int64) *apimodel.Sandbox {
		sb := &apimodel.Sandbox{ID: "sbx_1"}
		sb.Runtime.State = apiclientgen.SandboxRuntimeStateFailed
		sb.Runtime.ErrorMessage = apiclientgen.NewOptString(message)
		sb.Runtime.Generation, sb.Runtime.ObservedGeneration = generation, generation
		if repairGeneration != 0 {
			sb.Runtime.AutomaticRepair = apiclientgen.NewOptSandboxAutomaticRepair(apimodel.SandboxAutomaticRepair{
				Generation: repairGeneration,
				Trigger:    "exec attach",
				Failure:    apiclientgen.NewOptString("create container: EOF"),
			})
		}
		return sb
	}

	want := "pull image: not found\n" +
		"That is the one automatic repair of it failing; it had already failed with: create container: EOF\n" +
		"Run `discobox admin box repair sbx_1` to repair it again."
	if got := sandboxFailureReason(failed("pull image: not found", 5, 5)); got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
	if got := sandboxFailureReason(failed("pull image: not found", 5, 0)); got != "pull image: not found" {
		t.Fatalf("a failure nothing repaired = %q, want the message alone", got)
	}
	if got := sandboxFailureReason(failed("pull image: not found", 6, 5)); got != "pull image: not found" {
		t.Fatalf("a failure after an earlier repair = %q, want the message alone", got)
	}
	// A repair that landed is not this failure's: the box failed again later,
	// on the same generation.
	again := failed("no space left on device", 5, 5)
	repair := again.Runtime.AutomaticRepair.Value
	repair.LandedAt = apiclientgen.NewOptDateTime(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC))
	again.Runtime.AutomaticRepair = apiclientgen.NewOptSandboxAutomaticRepair(repair)
	if got := sandboxFailureReason(again); got != "no space left on device" {
		t.Fatalf("a failure after the repair landed = %q, want the message alone", got)
	}
	said := "the one automatic repair of it failed too; run `discobox admin box repair sbx_1`"
	if got := sandboxFailureReason(failed(said, 5, 5)); got != said {
		t.Fatalf("a message naming the command = %q, want it said once", got)
	}
}
