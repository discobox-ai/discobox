package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/winlsp"
)

// fakeLSP is Windows's record of categories, and the elevated process,
// without Windows.
type fakeLSP struct {
	categories map[string]uint32
	elevated   bool
	declined   bool
	// elevatedArgs is what the parent passed the elevated process.
	elevatedArgs []string
	// sets counts the categories written.
	sets int
}

func (f *fakeLSP) calls() lspCalls {
	calls := lspCalls{
		category: func(path string) (uint32, error) { return f.categories[path], nil },
		setCategory: func(path string, category uint32) error {
			f.sets++
			f.categories[path] = category
			return nil
		},
		elevated: func() bool { return f.elevated },
	}
	calls.runElevated = func(args []string) (uint32, error) {
		if f.declined {
			return 0, winlsp.ErrDeclined
		}
		f.elevatedArgs = args
		// The elevated process is this CLI: run the hidden command as it would.
		elevated := newServerLSPExemptElevatedCommand(calls)
		if len(args) < 3 || args[2] != lspExemptElevatedUse {
			return 2, nil
		}
		elevated.SetArgs(args[3:])
		elevated.SetOut(new(bytes.Buffer))
		elevated.SetErr(new(bytes.Buffer))
		if code, ok := ExitCode(elevated.Execute()); ok {
			return uint32(code), nil
		}
		return 0, nil
	}
	return calls
}

func fakeServerExe(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "discobox-server.exe")
	if err := os.WriteFile(path, []byte("server"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func runLSPExempt(t *testing.T, server string, fake *fakeLSP, check, remove bool) (string, error) {
	t.Helper()
	app := &App{serverSource: serverSource{binary: server}}
	var out bytes.Buffer
	err := app.lspExempt(context.Background(), &out, fake.calls(), check, remove)
	return out.String(), err
}

func TestLSPExemptMarksTheServerThroughTheElevatedStep(t *testing.T) {
	server := fakeServerExe(t)
	fake := &fakeLSP{categories: map[string]uint32{server: 0x1}}
	out, err := runLSPExempt(t, server, fake, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := fake.categories[server]; got != winlsp.LSPSystem|0x1 {
		t.Fatalf("category %#x, want LSPSystem added to the bits already there", got)
	}
	if want := []string{"admin", "server", lspExemptElevatedUse, "set", server}; strings.Join(fake.elevatedArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("elevated with %q, want %q", fake.elevatedArgs, want)
	}
	for _, want := range []string{"no longer pass through", "administrator rights", "is exempt", "discobox admin server shutdown", "applies to that path alone"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not say %q:\n%s", want, out)
		}
	}
}

func TestLSPExemptCallsDirectlyWhenAlreadyElevated(t *testing.T) {
	server := fakeServerExe(t)
	fake := &fakeLSP{categories: map[string]uint32{}, elevated: true}
	if _, err := runLSPExempt(t, server, fake, false, false); err != nil {
		t.Fatal(err)
	}
	if fake.elevatedArgs != nil {
		t.Fatal("an elevated prompt started another elevated process")
	}
	if fake.categories[server]&winlsp.LSPSystem == 0 {
		t.Fatal("not exempted")
	}
}

func TestLSPExemptChangesNothingAlreadyDone(t *testing.T) {
	server := fakeServerExe(t)
	fake := &fakeLSP{categories: map[string]uint32{server: winlsp.LSPSystem}}
	out, err := runLSPExempt(t, server, fake, false, false)
	if err != nil || fake.sets != 0 || fake.elevatedArgs != nil {
		t.Fatalf("exempting an exempt server: err %v, %d sets, elevated %v", err, fake.sets, fake.elevatedArgs)
	}
	if !strings.Contains(out, "already exempt") {
		t.Fatalf("output %q", out)
	}
	fake.categories[server] = 0
	if out, err = runLSPExempt(t, server, fake, false, true); err != nil || fake.sets != 0 {
		t.Fatalf("removing an absent exemption: err %v, %d sets", err, fake.sets)
	}
	if !strings.Contains(out, "nothing to remove") {
		t.Fatalf("output %q", out)
	}
}

func TestLSPExemptCheckOnlyReads(t *testing.T) {
	server := fakeServerExe(t)
	fake := &fakeLSP{categories: map[string]uint32{}}
	out, err := runLSPExempt(t, server, fake, true, false)
	if err != nil || fake.sets != 0 || fake.elevatedArgs != nil {
		t.Fatalf("--check: err %v, %d sets, elevated %v", err, fake.sets, fake.elevatedArgs)
	}
	if !strings.Contains(out, "is not exempt") {
		t.Fatalf("output %q", out)
	}
	fake.categories[server] = winlsp.LSPSystem
	if out, _ = runLSPExempt(t, server, fake, true, false); !strings.Contains(out, "is exempt") {
		t.Fatalf("output %q", out)
	}
}

func TestLSPExemptRemoveKeepsTheOtherBits(t *testing.T) {
	server := fakeServerExe(t)
	fake := &fakeLSP{categories: map[string]uint32{server: winlsp.LSPSystem | 0x4}}
	if _, err := runLSPExempt(t, server, fake, false, true); err != nil {
		t.Fatal(err)
	}
	if got := fake.categories[server]; got != 0x4 {
		t.Fatalf("category %#x after --remove, want 0x4", got)
	}
}

func TestLSPExemptReportsADeclinedPrompt(t *testing.T) {
	server := fakeServerExe(t)
	fake := &fakeLSP{categories: map[string]uint32{}, declined: true}
	_, err := runLSPExempt(t, server, fake, false, false)
	if !errors.Is(err, winlsp.ErrDeclined) || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err %v", err)
	}
}

// The result is what Windows reads back, not what the elevated step said.
func TestLSPExemptReportsWhatItReadsBack(t *testing.T) {
	server := fakeServerExe(t)
	fake := &fakeLSP{categories: map[string]uint32{}}
	calls := fake.calls()
	calls.runElevated = func([]string) (uint32, error) { return 0, nil }
	app := &App{serverSource: serverSource{binary: server}}
	err := app.lspExempt(context.Background(), new(bytes.Buffer), calls, false, false)
	if err == nil || !strings.Contains(err.Error(), "still not exempt") {
		t.Fatalf("an elevated step that changed nothing: %v", err)
	}
}

// The elevated step marks nothing but an existing discobox-server.exe named by
// an absolute path.
func TestApplyLSPExemptionRefusesAnythingElse(t *testing.T) {
	fake := &fakeLSP{categories: map[string]uint32{}}
	other := filepath.Join(t.TempDir(), "something.exe")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"discobox-server.exe",
		other,
		filepath.Join(t.TempDir(), "discobox-server.exe"),
		t.TempDir() + string(filepath.Separator) + "discobox-server.exe" + string(filepath.Separator),
	} {
		if err := applyLSPExemption(fake.calls(), path, true); err == nil {
			t.Errorf("%q was exempted", path)
		}
	}
	dir := filepath.Join(t.TempDir(), "discobox-server.exe")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := applyLSPExemption(fake.calls(), dir, true); err == nil {
		t.Error("a directory was exempted")
	}
	if fake.sets != 0 {
		t.Fatalf("%d categories written", fake.sets)
	}
	upper := filepath.Join(t.TempDir(), "DISCOBOX-SERVER.EXE")
	if err := os.WriteFile(upper, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyLSPExemption(fake.calls(), upper, true); err != nil {
		t.Fatalf("Windows names are case-insensitive: %v", err)
	}
}

// A staged server is marked at the fixed path every version runs from.
func TestLSPExemptTargetIsTheFixedRunPath(t *testing.T) {
	manifest, _ := servedManifest(t, "v9.9.9")
	root := t.TempDir()
	resolver := serverResolver{
		source:     serverSource{manifest: manifestFile(t, manifest)},
		executable: filepath.Join(fakeInstall(t, false), "discobox"),
		stageRoot:  root,
	}
	path, staged, err := resolver.lspExemptTarget(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if staged == nil || path != filepath.Join(manifest.RunDir(root), manifest.Command) {
		t.Fatalf("target %q (staged %v), want the run directory's command", path, staged != nil)
	}
}

func TestUninstallSaysAnExemptionStays(t *testing.T) {
	runDir := t.TempDir()
	path := filepath.Join(runDir, "discobox-server.exe")
	for _, name := range []string{"discobox-server.exe", "helper.dll"} {
		if err := os.WriteFile(filepath.Join(runDir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if kept := lspExemptionKept(runDir, func(string) (uint32, error) { return 0, nil }); kept != nil {
		t.Fatalf("an unexempted path is reported kept: %v", kept)
	}
	if kept := lspExemptionKept(filepath.Join(runDir, "absent"), func(string) (uint32, error) { return winlsp.LSPSystem, nil }); kept != nil {
		t.Fatalf("a run directory that is not there reports %v", kept)
	}
	kept := lspExemptionKept(runDir, func(p string) (uint32, error) {
		if p == path {
			return winlsp.LSPSystem, nil
		}
		return 0, nil
	})
	if len(kept) != 1 || kept[0].path != path || !strings.Contains(kept[0].reason, "lsp-exempt --remove") || !strings.Contains(kept[0].reason, "harmless") {
		t.Fatalf("kept %+v", kept)
	}
}

// A path the elevated step would refuse is refused before the prompt, where
// the person can read why.
func TestLSPExemptRefusesBeforeElevating(t *testing.T) {
	other := filepath.Join(t.TempDir(), "my-server.exe")
	if err := os.WriteFile(other, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := &fakeLSP{categories: map[string]uint32{}}
	_, err := runLSPExempt(t, other, fake, false, false)
	if err == nil || fake.elevatedArgs != nil {
		t.Fatalf("err %v, elevated with %q", err, fake.elevatedArgs)
	}
}

// What went wrong in the hidden elevated window reaches the person.
func TestLSPExemptSaysWhyTheElevatedStepFailed(t *testing.T) {
	server := fakeServerExe(t)
	fake := &fakeLSP{categories: map[string]uint32{}}
	calls := fake.calls()
	calls.setCategory = func(string, uint32) error { return errors.New("WSCSetApplicationCategory: access denied") }
	calls.runElevated = func(args []string) (uint32, error) {
		elevated := newServerLSPExemptElevatedCommand(calls)
		elevated.SetArgs(args[3:])
		elevated.SetOut(new(bytes.Buffer))
		elevated.SetErr(new(bytes.Buffer))
		if code, ok := ExitCode(elevated.Execute()); ok {
			return uint32(code), nil
		}
		return 0, nil
	}
	app := &App{serverSource: serverSource{binary: server}}
	err := app.lspExempt(context.Background(), new(bytes.Buffer), calls, false, false)
	if err == nil || !strings.Contains(err.Error(), "WSCSetApplicationCategory failed") {
		t.Fatalf("err %v, want what the elevated step's status means", err)
	}
}

// The elevated step refuses a path that is not a discobox-server.exe however
// it is reached, and says so with its status: it writes nothing anywhere else.
func TestLSPExemptElevatedRefusesWithAStatus(t *testing.T) {
	fake := &fakeLSP{categories: map[string]uint32{}}
	for args, want := range map[[2]string]int{
		{"set", filepath.Join(t.TempDir(), "other.exe")}: lspExemptExitRefused,
		{"flip", fakeServerExe(t)}:                       lspExemptExitUsage,
	} {
		cmd := newServerLSPExemptElevatedCommand(fake.calls())
		cmd.SetArgs(args[:])
		cmd.SetOut(new(bytes.Buffer))
		cmd.SetErr(new(bytes.Buffer))
		if code, ok := ExitCode(cmd.Execute()); !ok || code != want {
			t.Errorf("%q exited %d (%v), want %d", args, code, ok, want)
		}
	}
	if fake.sets != 0 {
		t.Fatalf("%d categories written", fake.sets)
	}
}

// The command's flags reach the flow: --check reads and changes nothing, and
// it cannot be combined with --remove.
func TestLSPExemptCommandFlags(t *testing.T) {
	server := fakeServerExe(t)
	fake := &fakeLSP{categories: map[string]uint32{}}
	app := &App{serverSource: serverSource{binary: server}}
	run := func(args ...string) (string, error) {
		cmd := app.newServerLSPExemptCommand(fake.calls())
		var out bytes.Buffer
		cmd.SetArgs(args)
		cmd.SetOut(&out)
		cmd.SetErr(new(bytes.Buffer))
		err := cmd.ExecuteContext(context.Background())
		return out.String(), err
	}
	if _, err := run("--check", "--remove"); err == nil {
		t.Fatal("--check and --remove together were accepted")
	}
	out, err := run("--check")
	if err != nil || fake.sets != 0 || !strings.Contains(out, "is not exempt") {
		t.Fatalf("--check: %v, %d sets, %q", err, fake.sets, out)
	}
}
