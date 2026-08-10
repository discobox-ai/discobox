package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestCreateSandboxBodyIncludesHarnessLaunchFields(t *testing.T) {
	t.Setenv("SANDBOX_ENV_FROM_SHELL", "from-shell")
	body, err := createSandboxBody(sandboxCreateOptions{
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
