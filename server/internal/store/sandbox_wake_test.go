package store_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// wakeStores is the store each wake test runs on: SQLite always, and the
// Postgres DISCOBOX_TEST_POSTGRES_DSN names when it is set, on a schema of its
// own. A wake is spent on the time it was read as, so the time has to come
// back from each engine as the value it compares equal to.
func wakeStores() map[string]func(t *testing.T) *store.Store {
	stores := map[string]func(t *testing.T) *store.Store{"sqlite": newTestStore}
	stores["postgres"] = func(t *testing.T) *store.Store {
		t.Helper()
		dsn := os.Getenv("DISCOBOX_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("set DISCOBOX_TEST_POSTGRES_DSN to run on Postgres")
		}
		ctx := context.Background()
		admin, err := database.New(database.Config{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = admin.Close() })
		schema := fmt.Sprintf("wake_%d", time.Now().UnixNano())
		if err := admin.Write.Exec("CREATE SCHEMA " + schema).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { admin.Write.Exec("DROP SCHEMA " + schema + " CASCADE") })
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		db, err := database.New(database.Config{DSN: dsn + sep + "search_path=" + schema})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		if err := db.Write.Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
			t.Fatal(err)
		}
		return store.New(db.Write, db.Read)
	}
	return stores
}

// A wake is spent on the epoch and the time it was read as, and only then: a
// wake written at a finer precision than the engine keeps still matches the
// value read back, and another time, or a moved epoch, is not that wake
// (ADR 26-11-15-829 §1).
func TestSpendSandboxWakeClearsOnlyTheWakeItRead(t *testing.T) {
	for engine, open := range wakeStores() {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			s := open(t)
			createTestPool(t, s, "project-1", "pool-1")
			if err := s.CreateSandbox(ctx, &model.Sandbox{ID: "sandbox-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "alpha"}); err != nil {
				t.Fatal(err)
			}
			// Nanoseconds: finer than Postgres keeps.
			wrote := time.Date(2026, 11, 15, 3, 36, 40, 123456789, time.UTC)
			if ok, err := s.SetSandboxWakeIfEpoch(ctx, "project-1", "sandbox-1", 0, &wrote); err != nil || !ok {
				t.Fatalf("set wake = %v, %v", ok, err)
			}
			read, err := s.GetSandbox(ctx, "project-1", "sandbox-1")
			if err != nil || read.WakeAt == nil {
				t.Fatalf("read wake = %v, %v", read, err)
			}
			if ok, err := s.SpendSandboxWake(ctx, "project-1", "sandbox-1", read.WakeEpoch, read.WakeAt.Add(time.Second)); err != nil || ok {
				t.Fatalf("spend of another wake = %v, %v, want nothing cleared", ok, err)
			}
			if ok, err := s.SpendSandboxWake(ctx, "project-1", "sandbox-1", read.WakeEpoch+1, *read.WakeAt); err != nil || ok {
				t.Fatalf("spend on a moved epoch = %v, %v, want nothing cleared", ok, err)
			}
			if ok, err := s.SpendSandboxWake(ctx, "project-1", "sandbox-1", read.WakeEpoch, *read.WakeAt); err != nil || !ok {
				t.Fatalf("spend of the wake read = %v, %v, want it cleared", ok, err)
			}
			after, err := s.GetSandbox(ctx, "project-1", "sandbox-1")
			if err != nil {
				t.Fatal(err)
			}
			if after.WakeAt != nil || after.WakeEpoch != read.WakeEpoch {
				t.Fatalf("after spend wake = %v, epoch %d; want none, epoch %d unmoved", after.WakeAt, after.WakeEpoch, read.WakeEpoch)
			}
		})
	}
}

// A pool's due wakes are its present sandboxes whose wake is at or before now.
func TestListSandboxIDsWithDueWake(t *testing.T) {
	for engine, open := range wakeStores() {
		t.Run(engine, func(t *testing.T) {
			ctx := context.Background()
			s := open(t)
			createTestPool(t, s, "project-1", "pool-1")
			createTestPool(t, s, "project-1", "pool-2")
			now := time.Date(2026, 11, 15, 4, 0, 0, 0, time.UTC)
			for _, sb := range []struct {
				id, pool, desired string
				wake              *time.Time
			}{
				{"due", "pool-1", model.DesiredStatePresent, ptrTime(now.Add(-time.Minute))},
				{"due-now", "pool-1", model.DesiredStatePresent, ptrTime(now)},
				{"ahead", "pool-1", model.DesiredStatePresent, ptrTime(now.Add(time.Minute))},
				{"none", "pool-1", model.DesiredStatePresent, nil},
				{"archived", "pool-1", model.DesiredStateArchived, ptrTime(now.Add(-time.Minute))},
				{"other-pool", "pool-2", model.DesiredStatePresent, ptrTime(now.Add(-time.Minute))},
			} {
				row := &model.Sandbox{ID: sb.id, ProjectID: "project-1", PoolID: sb.pool, CreatedByUserID: "user-1", Name: sb.id}
				row.DesiredState = sb.desired
				if err := s.CreateSandbox(ctx, row); err != nil {
					t.Fatal(err)
				}
				if sb.wake != nil {
					if _, err := s.SetSandboxWakeIfEpoch(ctx, "project-1", sb.id, 0, sb.wake); err != nil {
						t.Fatal(err)
					}
				}
			}
			ids, err := s.ListSandboxIDsWithDueWake(ctx, "project-1", "pool-1", now)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(ids, ",") != "due,due-now" {
				t.Fatalf("due wakes = %v, want due and due-now", ids)
			}
		})
	}
}

func ptrTime(at time.Time) *time.Time { return &at }
