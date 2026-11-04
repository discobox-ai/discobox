package database

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
)

func TestSandboxParentsUpgradeOnceAndPreserveRelease(t *testing.T) {
	db, err := New(Config{DSN: filepath.Join(t.TempDir(), "legacy.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A real legacy table, without the new column.
	if err := db.Write.Exec("CREATE TABLE sandboxes (id text PRIMARY KEY, project_id text, created_by_sandbox_id text)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Write.Exec("INSERT INTO sandboxes (id,project_id,created_by_sandbox_id) VALUES ('child','p','creator'),('creator','p',NULL),('person','p',NULL),('purged-child','p','purged'),('cross-child','other','creator')").Error; err != nil {
		t.Fatal(err)
	}
	if err := addSandboxParents(db.Write); err != nil {
		t.Fatal(err)
	}
	var children []model.Sandbox
	if err := db.Write.Select("id", "created_by_sandbox_id", "parent_sandbox_id").Where("id IN ?", []string{"child", "person"}).Order("id").Find(&children).Error; err != nil {
		t.Fatal(err)
	}
	if children[0].ParentSandboxID == nil || *children[0].ParentSandboxID != "creator" || children[1].ParentSandboxID != nil {
		t.Fatalf("backfill: %+v", children)
	}
	for _, id := range []string{"purged-child", "cross-child"} {
		var child model.Sandbox
		if err := db.Write.Select("id", "created_by_sandbox_id", "parent_sandbox_id").First(&child, "id = ?", id).Error; err != nil {
			t.Fatal(err)
		}
		if child.ParentSandboxID != nil || child.CreatedBySandboxID == nil {
			t.Fatalf("invalid creator became parent or lost audit: %+v", child)
		}
	}
	if err := db.Write.Exec("UPDATE sandboxes SET parent_sandbox_id = NULL WHERE id = 'child'").Error; err != nil {
		t.Fatal(err)
	}
	if err := addSandboxParents(db.Write.WithContext(context.Background())); err != nil {
		t.Fatal(err)
	}
	var child model.Sandbox
	if err := db.Write.Select("id", "created_by_sandbox_id", "parent_sandbox_id").First(&child, "id = ?", "child").Error; err != nil {
		t.Fatal(err)
	}
	if child.ParentSandboxID != nil || child.CreatedBySandboxID == nil || *child.CreatedBySandboxID != "creator" {
		t.Fatal("restart undid release or changed audit")
	}
}
