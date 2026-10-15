package skillssources_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/resources/skillssources"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func body(url, ref, dir string) services.CreateProjectSkillsSourceBody {
	b := services.CreateProjectSkillsSourceBody{URL: url}
	if ref != "" {
		b.Ref = apigen.NewOptString(ref)
	}
	if dir != "" {
		b.Directory = apigen.NewOptString(dir)
	}
	return b
}

func TestProjectSkillsSourcesKeepTheOrderAdded(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)

	first, err := svc.CreateProjectSkillsSource(ctx, "project-1", body("https://github.com/acme/tools", "", "agents/skills"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	second, err := svc.CreateProjectSkillsSource(ctx, "project-1", body("git@github.com:acme/shared.git", "v2", ""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if second.Directory != "." {
		t.Fatalf("directory = %q, want the root", second.Directory)
	}
	// Another directory of a repository the list names at the same ref is
	// one more entry in its checkout.
	third, err := svc.CreateProjectSkillsSource(ctx, "project-1", body("https://github.com/acme/tools", "", "extra"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	list, err := svc.ListProjectSkillsSources(ctx, "project-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 || list[0].ID != first.ID || list[1].ID != second.ID || list[2].ID != third.ID {
		t.Fatalf("list = %+v", list)
	}

	if err := svc.DeleteProjectSkillsSource(ctx, "project-1", second.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list, err = svc.ListProjectSkillsSources(ctx, "project-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 || list[0].ID != first.ID || list[1].ID != third.ID {
		t.Fatalf("list after delete = %+v", list)
	}
	if err := svc.DeleteProjectSkillsSource(ctx, "project-1", second.ID); statusOf(err) != http.StatusNotFound {
		t.Fatalf("delete again = %v, want 404", err)
	}
}

//nolint:gosec // G101: fixtures of a credential in a URL, which is what is refused.
func TestCreateProjectSkillsSourceRefuses(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	if _, err := svc.CreateProjectSkillsSource(ctx, "project-1", body("https://github.com/acme/tools", "main", "skills")); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, tc := range []struct {
		name string
		body services.CreateProjectSkillsSourceBody
		want int
	}{
		{"a directory", body("./skills", "", ""), http.StatusBadRequest},
		{"an absolute path", body("/home/me/skills", "", ""), http.StatusBadRequest},
		{"a file URL", body("file:///home/me/skills", "", ""), http.StatusBadRequest},
		{"a URL holding //", body("https://github.com/acme//tools", "", ""), http.StatusBadRequest},
		{"a ref holding @", body("https://github.com/acme/other", "a@b", ""), http.StatusBadRequest},
		{"a directory outside", body("https://github.com/acme/other", "", "../x"), http.StatusBadRequest},
		{"a URL Git reads as an option", body("--upload-pack=touch /tmp/x#@x:y", ".", ""), http.StatusBadRequest},
		{"a ref Git reads as an option", body("https://github.com/acme/other", "--upload-pack=x", ""), http.StatusBadRequest},
		{"a ref Git refuses", body("https://github.com/acme/other", ".", ""), http.StatusBadRequest},
		{"a URL with a token", body("https://x-access-token:ghp_secret@github.com/acme/other", "", ""), http.StatusBadRequest},
		{"an https URL with a user", body("https://ghp_secret@github.com/acme/other", "", ""), http.StatusBadRequest},
		{"the same entry twice", body("https://github.com/acme/tools", "main", "skills"), http.StatusConflict},
		{"the repository at another ref", body("https://github.com/acme/tools", "dev", "other"), http.StatusConflict},
		{"the repository at its default branch", body("https://github.com/acme/tools", "", "other"), http.StatusConflict},
		// One repository however its URL is spelled (ADR 26-10-15-664 §4).
		{"another spelling at another ref", body("https://github.com/Acme/tools.git", "dev", "other"), http.StatusConflict},
		{"its ssh URL at another ref", body("git@github.com:acme/tools.git", "dev", "other"), http.StatusConflict},
		{"another spelling of the same entry", body("https://github.com/acme/tools.git", "main", "skills"), http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateProjectSkillsSource(ctx, "project-1", tc.body)
			if got := statusOf(err); got != tc.want {
				t.Fatalf("status = %d (%v), want %d", got, err, tc.want)
			}
		})
	}
	if _, err := svc.CreateProjectSkillsSource(ctx, "project-missing", body("https://github.com/acme/tools", "", "")); statusOf(err) != http.StatusNotFound {
		t.Fatalf("missing project = %v, want 404", err)
	}
}

func statusOf(err error) int {
	var status interface{ StatusCode() int }
	if errors.As(err, &status) {
		return status.StatusCode()
	}
	return 0
}

func newTestService(t *testing.T) *skillssources.Service {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	return skillssources.NewService(store.New(db.Write, db.Read))
}
