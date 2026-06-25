package terminal

import (
	"context"
	"errors"
	"testing"

	"github.com/obot-platform/discobox/sandbox-agent/config"
)

func TestManagerCreateListDelete(t *testing.T) {
	runner := &fakeRunner{}
	manager, err := NewManager([]config.Agent{{
		ID:      "codex",
		Command: []string{"codex"},
	}}, "/workspace", t.TempDir(), runner, nil)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	created, err := manager.Create(context.Background(), CreateRequest{
		Args:     []string{"--resume"},
		Workdir:  "project",
		Metadata: map[string]string{"purpose": "test"},
	})
	if err != nil {
		t.Fatalf("create terminal: %v", err)
	}
	if created.Status != StatusRunning {
		t.Fatalf("status = %q", created.Status)
	}
	if created.Workdir != "/workspace/project" {
		t.Fatalf("workdir = %q", created.Workdir)
	}
	if got := runner.starts[0].Command; len(got) != 2 || got[0] != "codex" || got[1] != "--resume" {
		t.Fatalf("command = %#v", got)
	}

	listed := manager.List()
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("listed = %#v", listed)
	}
	if err := manager.Delete(context.Background(), created.ID); err != nil {
		t.Fatalf("delete terminal: %v", err)
	}
	if len(manager.List()) != 0 {
		t.Fatalf("terminal was not deleted")
	}
	if len(runner.stops) != 1 || runner.stops[0] != created.Unit {
		t.Fatalf("stops = %#v", runner.stops)
	}
}

func TestManagerRejectsWorkdirOutsideRoot(t *testing.T) {
	manager, err := NewManager([]config.Agent{{
		ID:      "codex",
		Command: []string{"codex"},
	}}, "/workspace", t.TempDir(), &fakeRunner{}, nil)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if _, err := manager.Create(context.Background(), CreateRequest{Workdir: "../etc"}); err == nil {
		t.Fatalf("expected workdir error")
	}
}

func TestManagerMarksFailedWhenRunnerFails(t *testing.T) {
	manager, err := NewManager([]config.Agent{{
		ID:      "codex",
		Command: []string{"codex"},
	}}, "/workspace", t.TempDir(), &fakeRunner{startErr: errors.New("boom")}, nil)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	terminal, err := manager.Create(context.Background(), CreateRequest{})
	if err == nil {
		t.Fatalf("expected start error")
	}
	if terminal.Status != StatusFailed {
		t.Fatalf("status = %q", terminal.Status)
	}
	listed := manager.List()
	if len(listed) != 1 || listed[0].Status != StatusFailed {
		t.Fatalf("listed = %#v", listed)
	}
}

type fakeRunner struct {
	starts   []StartRequest
	stops    []string
	startErr error
	stopErr  error
}

func (r *fakeRunner) Start(_ context.Context, req StartRequest) (StartResult, error) {
	r.starts = append(r.starts, req)
	if r.startErr != nil {
		return StartResult{}, r.startErr
	}
	return StartResult{Unit: req.Unit, PID: 1234, SkipStatusWait: true}, nil
}

func (r *fakeRunner) Stop(_ context.Context, unit string) error {
	r.stops = append(r.stops, unit)
	return r.stopErr
}

func (r *fakeRunner) Status(context.Context, string) (UnitStatus, error) {
	return UnitStatus{}, errors.New("not found")
}

func (r *fakeRunner) List(context.Context) ([]UnitStatus, error) {
	return nil, nil
}
