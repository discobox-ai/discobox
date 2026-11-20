package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/store"
)

func TestWorkSourceResetIsOrderedAndScopedToHostingPool(t *testing.T) {
	ctx := t.Context()
	s := newTestStore(t)
	createTestPool(t, s, "project-1", "pool-1")
	inventoryTestSandbox(t, s, "sandbox-1")
	allow := func(*store.Store) error { return nil }
	first := strings.Repeat("a", 40)
	second := strings.Repeat("b", 40)
	st, err := s.SetSourceReset(ctx, "project-1", "sandbox-1", "primary", first, "owner", 0, allow)
	if err != nil || st.Revision != 1 {
		t.Fatalf("first reset = %+v, %v", st, err)
	}
	if _, err := s.SetSourceReset(ctx, "project-1", "sandbox-1", "primary", second, "owner", 0, allow); !errors.Is(err, store.ErrSourceResetRevision) {
		t.Fatalf("superseded reset error = %v", err)
	}
	retry, err := s.SetSourceReset(ctx, "project-1", "sandbox-1", "primary", first, "owner", 0, allow)
	if err != nil || retry.Revision != 1 {
		t.Fatalf("retry = %+v, %v", retry, err)
	}
	pending, err := s.HostedSourceResets(ctx, "pool-1", "sandbox-1")
	if err != nil || pending["primary"].Commit != first {
		t.Fatalf("hosted reset = %+v, %v", pending, err)
	}
	foreign, err := s.HostedSourceResets(ctx, "another-pool", "sandbox-1")
	if err != nil || len(foreign) != 0 {
		t.Fatalf("foreign pool received %+v, %v", foreign, err)
	}
	if err := s.ObserveSourceResets(ctx, "sandbox-1", map[string]int64{"primary": 42}); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveSourceResets(ctx, "sandbox-1", map[string]int64{"primary": 1}); err != nil {
		t.Fatal(err)
	}
	applied, err := s.GetSourceReset(ctx, "sandbox-1", "primary")
	if err != nil || applied.AppliedRevision != 1 {
		t.Fatalf("applied reset = %+v, %v", applied, err)
	}
}
