package database_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func TestAuthorityNotificationsCrossDatabaseConnections(t *testing.T) {
	ctx := context.Background()
	dsn := "sqlite3://" + filepath.Join(t.TempDir(), "authority.db")
	writer, err := database.New(database.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	reader, err := database.New(database.Config{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, row := range []any{&model.Project{ID: "project", OwnerUserID: "user", Name: "project"}, &model.SandboxProviderInstance{ID: "provider", ProjectID: "project", Name: "provider", Type: "docker"}, &model.Pool{ID: "pool", ProjectID: "project", PoolManifest: model.PoolManifest{Name: "pool", ProviderInstanceID: "provider"}}, &model.Sandbox{ID: "lead", ProjectID: "project", PoolID: "pool", Name: "lead", DiscoboxAPI: true}, &model.Sandbox{ID: "child", ProjectID: "project", PoolID: "pool", Name: "child"}} {
		if err := writer.Write.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	observed := store.New(reader.Write, reader.Read)
	version := func(write bool) string {
		t.Helper()
		v, err := observed.ChildAuthorityVersion(ctx, "project", "lead", "child", write)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	initialRead, initialWrite := version(false), version(true)
	if err := writer.Write.Model(&model.Project{}).Where("id = ?", "project").Update("egress", `{"denyByDefault":true}`).Error; err != nil {
		t.Fatal(err)
	}
	if version(false) != initialRead {
		t.Fatal("egress mutation invalidated read authority")
	}
	if version(true) == initialWrite {
		t.Fatal("separate connection missed egress mutation")
	}
	// Two changes between reads must not disappear when the final state equals
	// the original one: a released capability cannot be resurrected this way.
	for _, allowed := range []bool{false, true} {
		if err := writer.Write.Model(&model.Sandbox{}).Where("id = ?", "lead").Update("discobox_api", allowed).Error; err != nil {
			t.Fatal(err)
		}
	}
	if version(false) == initialRead {
		t.Fatal("notification lost disable/re-enable between observations")
	}
}
