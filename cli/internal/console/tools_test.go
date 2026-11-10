package console

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// toolSource is a DataSource with one box's tools, recording what was run
// and edited.
type toolSource struct {
	DataSource
	tools   []Tool
	err     error
	hostErr error
	changed bool
	editErr error
	calls   []string
	execs   []Exec
}

func (s *toolSource) Execs(context.Context, string) ([]Exec, error) { return s.execs, nil }

func (s *toolSource) OpenExec(_ context.Context, box, exec string, _, _ int) (Terminal, error) {
	s.calls = append(s.calls, "open "+box+" "+exec)
	return nil, nil
}

func (s *toolSource) NewTool(_ context.Context, box, tool string, _, _ int) (Exec, Terminal, error) {
	s.calls = append(s.calls, "new "+box+" "+tool)
	return Exec{ID: "x_" + tool, Tool: tool, Live: true}, nil, nil
}

func (s *toolSource) Tools(context.Context, string) ([]Tool, error) { return s.tools, s.err }

func (s *toolSource) RunHostTool(_ context.Context, box, tool string) error {
	s.calls = append(s.calls, "host "+box+" "+tool)
	return s.hostErr
}

func (s *toolSource) EditToolFile(_ context.Context, file ToolFile, _ *Narration) (bool, error) {
	s.calls = append(s.calls, "edit "+file.Tool+" "+file.Name)
	return s.changed, s.editErr
}

func catalog() []Tool {
	return []Tool{
		{ID: "diff", Label: "Diff"},
		{ID: "fresh", Label: "Fresh", Problem: "refused: shadows the CLI's"},
		{ID: "fresh", Label: "Fresh", Files: []ToolFile{{Tool: "fresh", Name: "config.json", Home: ".config/fresh/config.json"}}},
		{ID: "ide", Label: "IDE", Host: true},
		{ID: "broken", Label: "Broken", Problem: "its command is empty"},
	}
}

// The picker lists a refused declaration only when nothing under its id
// runs, and an id finds the one that runs.
func TestPickerToolsAndToolInPreferTheOneThatRuns(t *testing.T) {
	var ids []string
	for _, tl := range PickerTools(catalog()) {
		ids = append(ids, tl.ID+":"+tl.Problem)
	}
	if want := []string{"diff:", "fresh:", "ide:", "broken:its command is empty"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("picker %q, want %q", ids, want)
	}
	if tl, ok := ToolIn(catalog(), "fresh"); !ok || tl.Problem != "" || len(tl.Files) != 1 {
		t.Fatalf("fresh %+v", tl)
	}
	if tl, ok := ToolIn(catalog(), "broken"); !ok || tl.Problem == "" {
		t.Fatalf("a refused declaration alone is still found: %+v", tl)
	}
	if _, ok := ToolIn(catalog(), "gone"); ok {
		t.Fatal("found a tool nobody declares")
	}
}

// A tool cannot run for its declaration's problem first, then for its box.
func TestAToolSaysWhyItCannotRun(t *testing.T) {
	up := Sandbox{ID: "b1", State: StateRunning, HasRuntime: true}
	down := Sandbox{ID: "b1", State: StateStopped}
	if why := (Tool{}).Why(up); why != "" {
		t.Fatalf("a tool on a box that is up: %q", why)
	}
	if why := (Tool{}).Why(down); why == "" {
		t.Fatal("a tool on a stopped box can run")
	}
	if why := (Tool{Problem: "no"}).Why(down); why != "no" {
		t.Fatalf("problem first: %q", why)
	}
	if got := (Tool{Detail: "a viewer"}).PickerDetail(true); got != "a viewer · running" {
		t.Fatalf("running: %q", got)
	}
}

// A host tool is run as the picker offers it, read again: what cannot run is
// refused before anything is handed the box, and a failure carries the id.
func TestRunHostToolIsOffered(t *testing.T) {
	ctx := context.Background()
	up := Sandbox{ID: "b1", Name: "work", State: StateRunning, HasRuntime: true}
	src := &toolSource{tools: catalog()}
	if note, err := RunHostTool(ctx, src, up, "ide"); err != nil || note != "opened work in IDE" {
		t.Fatalf("ran %q, %v", note, err)
	}
	for _, id := range []string{"diff", "broken", "gone"} {
		if _, err := RunHostTool(ctx, src, up, id); err == nil {
			t.Errorf("%s ran", id)
		}
	}
	if _, err := RunHostTool(ctx, src, Sandbox{ID: "b1", State: StateStopped}, "ide"); err == nil {
		t.Error("ran against a stopped box")
	}
	if len(src.calls) != 1 {
		t.Fatalf("calls %q, want only the one offered", src.calls)
	}
	src.hostErr = errors.New("no IDE found")
	if _, err := RunHostTool(ctx, src, up, "ide"); err == nil || err.Error() != "ide: no IDE found" {
		t.Fatalf("a failure: %v", err)
	}
}

// A tool's file is edited when the tool carries it, whatever the box's
// state: the copy is this machine's.
func TestEditToolEditsAFileTheToolCarries(t *testing.T) {
	ctx := context.Background()
	down := Sandbox{ID: "b1", State: StateStopped}
	src := &toolSource{tools: catalog(), changed: true}
	note, err := EditTool(ctx, src, down, "fresh", "config.json", nil)
	if err != nil || note != "config.json saved — the next discobox to open fresh gets it" {
		t.Fatalf("edit %q, %v", note, err)
	}
	src.changed = false
	if note, _ := EditTool(ctx, src, down, "fresh", "config.json", nil); note != "config.json unchanged" {
		t.Fatalf("unchanged: %q", note)
	}
	for _, c := range [][2]string{{"fresh", "nope"}, {"diff", "x"}, {"gone", "x"}} {
		if _, err := EditTool(ctx, src, down, c[0], c[1], nil); err == nil {
			t.Errorf("%v edited", c)
		}
	}
	if _, err := EditTool(ctx, src, down, "diff", "x", nil); err == nil || err.Error() != "Diff carries no config" {
		t.Fatalf("no files: %v", err)
	}
	src.editErr = context.Canceled
	if _, err := EditTool(ctx, src, down, "fresh", "config.json", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("given up: %v", err)
	}
}

// An address is its value, or why there is none.
func TestAddressOffers(t *testing.T) {
	up := Sandbox{ID: "b1", State: StateRunning, HasRuntime: true}
	whys := func(o []AddressOffer) [2]string {
		return [2]string{o[0].Value + o[0].Why, o[1].Value + o[1].Why}
	}
	for _, c := range []struct {
		box   Sandbox
		addr  Addresses
		err   error
		known bool
		want  [2]string
	}{
		{up, Addresses{SSH: "ssh b1", Git: "ssh://b1/w"}, nil, true, [2]string{"ssh b1", "ssh://b1/w"}},
		{up, Addresses{SSH: "ssh b1"}, nil, true, [2]string{"ssh b1", "nothing to reach it by"}},
		{up, Addresses{}, nil, false, [2]string{"looking it up…", "looking it up…"}},
		{up, Addresses{}, errors.New("no config"), true, [2]string{"no config", "no config"}},
	} {
		if got := whys(AddressOffers(c.box, c.addr, c.err, c.known)); got != c.want {
			t.Errorf("%+v: %q, want %q", c, got, c.want)
		}
	}
	if o := AddressOffers(Sandbox{ID: "b1", State: StateStopped}, Addresses{SSH: "ssh b1"}, nil, true); o[0].Value != "" || o[0].Why == "" {
		t.Fatalf("a stopped box's address %+v", o[0])
	}
}

// A tool's session is its live one when the box is running it, and made
// otherwise; one on this machine, or one that cannot run, has none.
func TestOpenToolShowsARunningToolsSession(t *testing.T) {
	ctx := context.Background()
	up := Sandbox{ID: "b1", State: StateRunning, HasRuntime: true}
	src := &toolSource{tools: catalog(), execs: []Exec{
		{ID: "e_diff", Tool: "diff", Live: true},
		{ID: "e_old", Tool: "fresh", Live: false},
		{ID: "sh", Live: true},
	}}
	if _, e, _, err := OpenTool(ctx, src, up, "diff", 80, 24); err != nil || e.ID != "e_diff" {
		t.Fatalf("diff: %+v, %v", e, err)
	}
	if _, e, _, err := OpenTool(ctx, src, up, "fresh", 80, 24); err != nil || e.ID != "x_fresh" {
		t.Fatalf("fresh: %+v, %v", e, err)
	}
	for _, id := range []string{"vscode", "ide", "broken", "gone"} {
		if _, _, _, err := OpenTool(ctx, src, up, id, 80, 24); err == nil {
			t.Errorf("%s opened", id)
		}
	}
	if want := []string{"open b1 e_diff", "new b1 fresh"}; !reflect.DeepEqual(src.calls, want) {
		t.Fatalf("calls %q, want %q", src.calls, want)
	}
	if got := RunningTools(src.execs); len(got) != 1 || got["diff"].ID != "e_diff" {
		t.Fatalf("running %+v", got)
	}
}
