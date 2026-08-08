package terminal

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/obot-platform/discobox/sandbox-agent/config"
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

	// A primary whose unit vanished with a reboot is not a terminal anyone can
	// attach to, so it must not shadow a live one — and EnsurePrimary, which
	// skips the relaunch only for a starting/running primary, must relaunch.
	t.Run("lost primary does not count as live", func(t *testing.T) {
		list := []execs.Exec{
			primaryExec("lost", execs.StatusLost, base.Add(time.Hour)),
			primaryExec("live", execs.StatusRunning, base),
		}
		got, ok := selectLivePrimary(list)
		if !ok || got.ID != "live" {
			t.Fatalf("selectLivePrimary = %q,%v, want live,true", got.ID, ok)
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

// A terminal is an exec, so it must run as the exec primitive's default user --
// groups included. The terminal layer used to rebuild that identity from
// ExecDefaults and drop AdditionalGroups, so the primary terminal launched at
// sandbox start/resume lost every group the image declared (e.g. "docker")
// while plain execs kept them.
func TestPrimaryTerminalRunsWithTheExecDefaultUsersGroups(t *testing.T) {
	dir := t.TempDir()
	uid := int64(1000)
	gid := int64(1000)
	units := &fakeUnits{}
	execManager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot: dir,
		RuntimeDir:  filepath.Join(dir, "rt"),
		Env:         map[string]string{"PATH": "/usr/bin"},
		Units:       units,
		DefaultUser: &execs.User{
			Name:             "dev",
			UID:              &uid,
			GID:              &gid,
			HomeDirectory:    "/home/dev",
			AdditionalGroups: []string{"docker"},
		},
	})
	if err != nil {
		t.Fatalf("new exec manager: %v", err)
	}
	svc, err := NewService(ServiceConfig{
		Execs:       execManager,
		WorkingRoot: dir,
		RuntimeDir:  filepath.Join(dir, "rt"),
		Env:         map[string]string{"PATH": "/usr/bin"},
		Harness:     config.Harness{ID: "codex", Command: []string{"codex"}},
		Units:       units,
		Installer:   &noopInstaller{},
		// Wiring passes ExecDefaults for the file installer; the run identity
		// must not be re-derived from it.
		ExecDefaults: config.ExecDefaults{Username: "dev", UID: &uid, GID: &gid, HomeDirectory: "/home/dev"},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	ex, err := svc.Create(context.Background(), CreateRequest{primary: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !IsPrimary(ex) {
		t.Fatalf("expected a primary terminal, metadata=%v", ex.Metadata)
	}
	if ex.User == nil {
		t.Fatal("primary terminal ran with no user")
	}
	if got := ex.User.AdditionalGroups; len(got) != 1 || got[0] != "docker" {
		t.Fatalf("additionalGroups = %v, want [docker]", got)
	}
}
