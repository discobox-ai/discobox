package terminal

import (
	"testing"
	"time"

	"github.com/obot-platform/discobox/sandbox-agent/execs"
)

func primaryExec(id string, status execs.Status, created time.Time) execs.Exec {
	return execs.Exec{ID: id, Status: status, CreatedAt: created, Metadata: map[string]string{metadataPrimary: "true"}}
}

func TestSelectLivePrimary(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nonPrimary := execs.Exec{ID: "other", Status: execs.StatusRunning}

	t.Run("prefers running primary over exited", func(t *testing.T) {
		list := []execs.Exec{
			primaryExec("old", execs.StatusExited, base),
			nonPrimary,
			primaryExec("live", execs.StatusRunning, base.Add(time.Minute)),
		}
		got, ok := selectLivePrimary(list)
		if !ok || got.ID != "live" {
			t.Fatalf("selectLivePrimary = %q,%v, want live,true", got.ID, ok)
		}
	})

	t.Run("falls back to newest exited primary", func(t *testing.T) {
		list := []execs.Exec{
			primaryExec("old", execs.StatusExited, base),
			primaryExec("new", execs.StatusFailed, base.Add(time.Hour)),
		}
		got, ok := selectLivePrimary(list)
		if !ok || got.ID != "new" {
			t.Fatalf("selectLivePrimary = %q,%v, want new,true", got.ID, ok)
		}
	})

	t.Run("none when no primary", func(t *testing.T) {
		if _, ok := selectLivePrimary([]execs.Exec{nonPrimary}); ok {
			t.Fatal("expected no primary")
		}
	})
}

func TestPrimaryExecIDConst(t *testing.T) {
	if PrimaryExecID != "primary" {
		t.Fatalf("PrimaryExecID = %q, want primary", PrimaryExecID)
	}
}
