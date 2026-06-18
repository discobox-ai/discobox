package agent_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/obot-platform/discobox/prompter/internal/agent"
)

func TestSessionStorePutGetAndDelete(t *testing.T) {
	store := agent.SessionStore{Path: filepath.Join(t.TempDir(), "sessions.json")}
	workdir := t.TempDir()
	updatedAt := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)

	record := agent.SessionRecord{
		Agent:             agent.KindCodex,
		Workdir:           workdir,
		CallerSessionID:   "user-session-1",
		ProviderSessionID: "provider-session-1",
		UpdatedAt:         updatedAt,
	}
	if err := store.Put(record); err != nil {
		t.Fatalf("put session: %v", err)
	}

	got, ok, err := store.Get(agent.KindCodex, workdir, "user-session-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if !ok {
		t.Fatal("expected session mapping")
	}
	if got.ProviderSessionID != "provider-session-1" {
		t.Fatalf("expected provider session, got %#v", got)
	}
	if !got.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("expected updatedAt %s, got %s", updatedAt, got.UpdatedAt)
	}

	record.ProviderSessionID = "provider-session-2"
	if err := store.Put(record); err != nil {
		t.Fatalf("replace session: %v", err)
	}
	got, ok, err = store.Get(agent.KindCodex, workdir, "user-session-1")
	if err != nil {
		t.Fatalf("get replaced session: %v", err)
	}
	if !ok || got.ProviderSessionID != "provider-session-2" {
		t.Fatalf("expected replaced provider session, ok=%v got=%#v", ok, got)
	}

	if err := store.Delete(agent.KindCodex, workdir, "user-session-1"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	_, ok, err = store.Get(agent.KindCodex, workdir, "user-session-1")
	if err != nil {
		t.Fatalf("get deleted session: %v", err)
	}
	if ok {
		t.Fatal("expected mapping to be deleted")
	}
}

func TestSessionStoreScopesByAgentAndWorkdir(t *testing.T) {
	store := agent.SessionStore{Path: filepath.Join(t.TempDir(), "sessions.json")}
	workdirA := t.TempDir()
	workdirB := t.TempDir()

	for _, record := range []agent.SessionRecord{
		{Agent: agent.KindCodex, Workdir: workdirA, CallerSessionID: "s1", ProviderSessionID: "codex-a"},
		{Agent: agent.KindCodex, Workdir: workdirB, CallerSessionID: "s1", ProviderSessionID: "codex-b"},
		{Agent: agent.KindOpenCode, Workdir: workdirA, CallerSessionID: "s1", ProviderSessionID: "opencode-a"},
	} {
		if err := store.Put(record); err != nil {
			t.Fatalf("put %#v: %v", record, err)
		}
	}

	tests := []struct {
		agent   agent.Kind
		workdir string
		want    string
	}{
		{agent.KindCodex, workdirA, "codex-a"},
		{agent.KindCodex, workdirB, "codex-b"},
		{agent.KindOpenCode, workdirA, "opencode-a"},
	}
	for _, test := range tests {
		got, ok, err := store.Get(test.agent, test.workdir, "s1")
		if err != nil {
			t.Fatalf("get %s %s: %v", test.agent, test.workdir, err)
		}
		if !ok || got.ProviderSessionID != test.want {
			t.Fatalf("expected %s, ok=%v got=%#v", test.want, ok, got)
		}
	}
}

func TestSessionStoreConcurrentPutsPreserveMappings(t *testing.T) {
	store := agent.SessionStore{Path: filepath.Join(t.TempDir(), "sessions.json")}
	workdir := t.TempDir()

	const count = 32
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.Put(agent.SessionRecord{
				Agent:             agent.KindCodex,
				Workdir:           workdir,
				CallerSessionID:   fmt.Sprintf("caller-%02d", i),
				ProviderSessionID: fmt.Sprintf("provider-%02d", i),
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("put session concurrently: %v", err)
		}
	}

	for i := range count {
		callerSessionID := fmt.Sprintf("caller-%02d", i)
		wantProviderSessionID := fmt.Sprintf("provider-%02d", i)
		got, ok, err := store.Get(agent.KindCodex, workdir, callerSessionID)
		if err != nil {
			t.Fatalf("get %s: %v", callerSessionID, err)
		}
		if !ok || got.ProviderSessionID != wantProviderSessionID {
			t.Fatalf("expected %s => %s, ok=%v got=%#v", callerSessionID, wantProviderSessionID, ok, got)
		}
	}
}

func TestSessionStoreRejectsEmptyCallerSessionID(t *testing.T) {
	store := agent.SessionStore{Path: filepath.Join(t.TempDir(), "sessions.json")}
	if err := store.Put(agent.SessionRecord{Agent: agent.KindCodex, Workdir: t.TempDir(), ProviderSessionID: "provider"}); !errors.Is(err, agent.ErrEmptySessionID) {
		t.Fatalf("expected ErrEmptySessionID from Put, got %v", err)
	}
	_, _, err := store.Get(agent.KindCodex, t.TempDir(), "")
	if !errors.Is(err, agent.ErrEmptySessionID) {
		t.Fatalf("expected ErrEmptySessionID from Get, got %v", err)
	}
	if err := store.Delete(agent.KindCodex, t.TempDir(), ""); !errors.Is(err, agent.ErrEmptySessionID) {
		t.Fatalf("expected ErrEmptySessionID from Delete, got %v", err)
	}
}
