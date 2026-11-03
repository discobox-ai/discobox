package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/execstream/client"
	"github.com/discobox-ai/discobox/pkg/execstream/frame"
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
	if got := (&App{leaderKey: "alt+g"}).detachHint(); got != "Alt-G d" {
		t.Errorf("alt+g detach hint = %q, want %q", got, "Alt-G d")
	}
}

// An Alt leader reaches a raw attach as Escape and then the letter, two bytes
// that nothing says arrive in one read. The chord has to be found across reads,
// and an Escape that turns out not to be it has to pass through untouched.
func TestTerminalDetachFilterWithAnAltLeader(t *testing.T) {
	const esc = 0x1b
	f := newDetachFilter("alt+g")

	var forwarded []byte
	for _, read := range [][]byte{{'h', 'i', esc}, {'g'}, {'x'}} {
		out, detach := f.filter(read)
		if detach {
			t.Fatalf("Alt-G then x detached")
		}
		forwarded = append(forwarded, out...)
	}
	if want := []byte{'h', 'i', esc, 'g', 'x'}; string(forwarded) != string(want) {
		t.Fatalf("Alt-G then x forwarded %q, want %q", forwarded, want)
	}

	f = newDetachFilter("alt+g")
	forwarded = nil
	detached := false
	for _, read := range [][]byte{{'h', 'i', esc}, {'g'}, {'d'}} {
		out, detach := f.filter(read)
		forwarded = append(forwarded, out...)
		detached = detached || detach
	}
	if !detached || string(forwarded) != "hi" {
		t.Fatalf("Alt-G d across reads: detach=%t forwarded=%q, want detach and %q", detached, forwarded, "hi")
	}

	// An arrow key is Escape too, and is not the leader.
	f = newDetachFilter("alt+g")
	if out, detach := f.filter([]byte{esc, '[', 'A'}); detach || string(out) != "\x1b[A" || f.undecided() {
		t.Fatalf("up arrow = %q detach=%t, want it passed through", out, detach)
	}

	// Alt held through the pair sends Alt-D, which detaches too, however its
	// two bytes are split.
	for _, reads := range [][][]byte{
		{{esc, 'g', esc, 'd'}},
		{{esc}, {'g', esc}, {'d'}},
	} {
		f = newDetachFilter("alt+g")
		detached = false
		forwarded = nil
		for _, read := range reads {
			out, detach := f.filter(read)
			forwarded = append(forwarded, out...)
			detached = detached || detach
		}
		if !detached || len(forwarded) != 0 {
			t.Fatalf("Alt-G Alt-D in reads %q: detach=%t forwarded=%q", reads, detached, forwarded)
		}
	}

	// The leader twice is how you type one, and Escape twice is two Escapes,
	// the second of which might still begin the leader.
	f = newDetachFilter("alt+g")
	if out, detach := f.filter([]byte{esc, 'g', esc, 'g', 'z'}); detach || string(out) != "\x1bgz" {
		t.Fatalf("Alt-G twice = %q detach=%t, want one literal Alt-G", out, detach)
	}
	f = newDetachFilter("alt+g")
	out, detach := f.filter([]byte{esc, esc})
	if detach || string(out) != "\x1b" || !f.undecided() {
		t.Fatalf("Escape Escape = %q detach=%t undecided=%t, want the first sent and the second held", out, detach, f.undecided())
	}

	// Another Alt chord is the program's, at once.
	f = newDetachFilter("alt+g")
	if out, detach := f.filter([]byte{esc, 'b'}); detach || string(out) != "\x1bb" || f.undecided() {
		t.Fatalf("Alt-B = %q detach=%t, want it passed through", out, detach)
	}
	// And so is a Ctrl leader's chord under an Alt leader.
	f = newDetachFilter("alt+g")
	if out, detach := f.filter([]byte{0x01, 'd'}); detach || string(out) != "\x01d" {
		t.Fatalf("Ctrl-A d under alt+g = %q detach=%t, want it passed through", out, detach)
	}
}

// What the filter is still holding when the input goes quiet is decided by
// flushing it: a lone Escape was the Escape key, and an armed leader with an
// Escape after it is delivered with it, as a leader that qualified nothing is.
func TestTerminalDetachFilterFlushesALoneEscape(t *testing.T) {
	const esc = 0x1b
	f := newDetachFilter("alt+g")
	if out, _ := f.filter([]byte{'i', esc}); string(out) != "i" || !f.undecided() {
		t.Fatalf("i Escape = %q undecided=%t, want the Escape held", out, f.undecided())
	}
	if got := f.flush(); string(got) != "\x1b" || f.undecided() {
		t.Fatalf("flush = %q undecided=%t, want the Escape", got, f.undecided())
	}
	// Flushing decided it, so the next g is a g.
	if out, detach := f.filter([]byte{'g'}); detach || string(out) != "g" {
		t.Fatalf("g after the flush = %q detach=%t", out, detach)
	}

	f = newDetachFilter("alt+g")
	f.filter([]byte{esc, 'g', esc})
	if got := f.flush(); string(got) != "\x1bg\x1b" || f.armed {
		t.Fatalf("Alt-G Escape flushed = %q armed=%t, want both delivered", got, f.armed)
	}

	// The armed leader alone is not undecided: it waits for its key however
	// long that takes.
	f = newDetachFilter("alt+g")
	f.filter([]byte{esc, 'g'})
	if f.undecided() || f.flush() != nil || !f.armed {
		t.Fatalf("an armed leader should wait, not flush")
	}
	// Nor is anything under a Ctrl leader, whose keys are one byte each.
	f = newDetachFilter("ctrl+a")
	f.filter([]byte{esc})
	if f.undecided() {
		t.Fatalf("Escape under ctrl+a should be sent at once")
	}
}

// recordingConn is the remote end of an exec stream as a list of the input it
// was sent.
type recordingConn struct {
	mu     sync.Mutex
	inputs [][]byte
}

func (c *recordingConn) ReadFrame() (frame.Frame, error) { select {} }
func (c *recordingConn) Close() error                    { return nil }
func (c *recordingConn) WriteFrame(typ byte, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if typ == frame.Input {
		c.inputs = append(c.inputs, append([]byte(nil), payload...))
	}
	return nil
}

func (c *recordingConn) sent() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(bytes.Join(c.inputs, nil))
}

// A lone Escape under an Alt leader reaches the program once the escape time
// is up, without waiting for a keystroke that may never come — vim's way out of
// insert mode is that key, pressed and then nothing.
func TestTerminalInputSendsALoneEscapeAfterTheEscapeTime(t *testing.T) {
	stdin, typed := io.Pipe()
	conn := &recordingConn{}
	s := client.New(client.Options{Conn: conn, Stdin: stdin})
	done := make(chan error, 1)
	go func() { done <- copyTerminalInput(context.Background(), s, newDetachFilter("alt+g")) }()

	if _, err := typed.Write([]byte{'i', 0x1b}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for conn.sent() != "i\x1b" {
		if time.Now().After(deadline) {
			t.Fatalf("sent %q, want the Escape sent on its own once the escape time passed", conn.sent())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The flush decided the held Escape and nothing else: the chord after it
	// still detaches. (Across reads is the filter's; see
	// TestTerminalDetachFilterWithAnAltLeader.)
	if _, err := typed.Write([]byte{0x1b, 'g', 'd'}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, errTerminalDetached) {
		t.Fatalf("copyTerminalInput = %v, want detached", err)
	}
	if got := conn.sent(); got != "i\x1b" {
		t.Fatalf("sent %q, want nothing of the chord", got)
	}
}

// Input that ends with half a leader held sends it: nothing more is coming to
// finish it.
func TestTerminalInputSendsWhatIsHeldWhenInputEnds(t *testing.T) {
	conn := &recordingConn{}
	s := client.New(client.Options{Conn: conn, Stdin: bytes.NewReader([]byte{'q', 0x1b})})
	if err := copyTerminalInput(context.Background(), s, newDetachFilter("alt+g")); err != nil {
		t.Fatal(err)
	}
	if got := conn.sent(); got != "q\x1b" {
		t.Fatalf("sent %q, want q and the Escape", got)
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

func TestUpdateHarnessPreservesSelectorAndRejectsEmpty(t *testing.T) {
	for _, value := range []string{"codex", "My Codex", "harness_123", " "} {
		cmd := &cobra.Command{}
		opts := sandboxUpdateOptions{}
		addUpdateFlags(cmd, &opts)
		if err := cmd.Flags().Set("harness", value); err != nil {
			t.Fatal(err)
		}
		change, err := sandboxChangeFromFlags(cmd, opts)
		if value == " " {
			if err == nil {
				t.Fatal("accepted empty harness")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := change.body(nil)
		if err != nil {
			t.Fatal(err)
		}
		config, _ := body.Config.Get()
		if config.Harness.Or("") != value || config.HarnessConfigId.IsSet() {
			t.Fatalf("selector rewritten: %+v", config)
		}
	}
}

// The console names a harness by its config ID, which is sent as
// harnessConfigId: the selector --harness takes is matched by slug and name
// before ID, and could pick another harness.
func TestSandboxChangeSendsAConfigIDExactly(t *testing.T) {
	id := "harness_123"
	body, err := sandboxChange{harnessConfigID: &id}.body(nil)
	if err != nil {
		t.Fatal(err)
	}
	config, _ := body.Config.Get()
	if config.HarnessConfigId.Or("") != id || config.Harness.IsSet() {
		t.Fatalf("config = %+v", config)
	}
}
