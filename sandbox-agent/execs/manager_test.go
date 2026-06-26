package execs

import (
	"context"
	"errors"
	"testing"
)

func TestManagerMergesConfigEnvWithRequestOverrides(t *testing.T) {
	runner := &fakeUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Env:         map[string]string{"BASE": "sandbox", "OVERRIDE": "sandbox"},
		Units:       runner,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if _, err := manager.Create(context.Background(), CreateRequest{
		Command: []string{"echo", "ok"},
		Env:     map[string]string{"OVERRIDE": "exec", "LOCAL": "exec"},
	}); err != nil {
		t.Fatalf("create exec: %v", err)
	}

	env := runner.starts[0].Env
	if env["BASE"] != "sandbox" || env["OVERRIDE"] != "exec" || env["LOCAL"] != "exec" {
		t.Fatalf("env = %#v, want config env with request override", env)
	}
}

type fakeUnitManager struct {
	starts []StartRequest
}

func (m *fakeUnitManager) Start(_ context.Context, req StartRequest) (StartResult, error) {
	m.starts = append(m.starts, req)
	return StartResult{Unit: req.Unit, PID: 1234}, nil
}

func (m *fakeUnitManager) Status(context.Context, string) (UnitStatus, error) {
	return UnitStatus{}, errors.New("not found")
}

func (m *fakeUnitManager) List(context.Context) ([]UnitStatus, error) {
	return nil, nil
}
