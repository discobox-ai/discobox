package judges

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// fakeSandboxes stands in for the sandbox service: it records what the judge's
// convergence asked for and keeps the rows the store would.
type fakeSandboxes struct {
	store   *store.Store
	created []*model.Sandbox
	deleted []string
	err     error
}

func (f *fakeSandboxes) CreateSandbox(ctx context.Context, projectID string, input services.CreateSandboxBody) (*model.Sandbox, error) {
	if f.err != nil {
		return nil, f.err
	}
	sandbox := &model.Sandbox{
		ID:        "sb_" + input.Config.Name,
		ProjectID: projectID,
		PoolID:    input.PoolId.Or(""),
		Name:      input.Config.Name,
		SandboxManifest: model.SandboxManifest{
			HarnessMode: string(input.Config.HarnessMode.Or("")),
		},
	}
	if id, ok := input.Config.HarnessConfigId.Get(); ok {
		sandbox.HarnessConfigID = &id
	}
	if sandbox.HarnessConfigID != nil {
		if config, err := f.store.GetHarnessConfig(ctx, projectID, *sandbox.HarnessConfigID); err == nil {
			sandbox.Image = config.Image
			sandbox.ImageDigest = config.ImageDigest
		}
	}
	if err := f.store.CreateSandbox(ctx, sandbox); err != nil {
		return nil, err
	}
	f.created = append(f.created, sandbox)
	return sandbox, nil
}

// PurgeSandbox destroys the row, as the sandbox service's does: a judge is
// purged rather than archived, because an archived discobox keeps the pool it
// ran in and would hold that pool open for good.
func (f *fakeSandboxes) PurgeSandbox(ctx context.Context, projectID, sandboxID string) error {
	f.deleted = append(f.deleted, sandboxID)
	return f.store.DeleteSandbox(ctx, projectID, sandboxID)
}

// archive is what somebody deleting the judge by its ID does, which is not the
// same as this package taking it away.
func (f *fakeSandboxes) archive(ctx context.Context, projectID, sandboxID string) error {
	sandbox, err := f.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return err
	}
	sandbox.DesiredState = model.DesiredStateArchived
	return f.store.UpdateSandbox(ctx, sandbox)
}

func newJudgeTest(t *testing.T) (*Service, *store.Store, *fakeSandboxes) {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	appStore := store.New(db.Write, db.Read)
	if err := db.Write.WithContext(ctx).Create(&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "Project"}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.SandboxProviderInstance{
		ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "Docker",
	}).Error; err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if err := appStore.CreatePool(ctx, &model.Pool{
		ID: "pool-1", ProjectID: "project-1",
		PoolManifest: model.PoolManifest{Name: "Default", ProviderInstanceID: "provider-1"},
	}); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	sandboxes := &fakeSandboxes{store: appStore}
	// Enabled: every test below is about what a server that judges does. The
	// server that has not opted in is its own test.
	return New(appStore, sandboxes, nil, true), appStore, sandboxes
}

// harness records a configured harness and makes it the project's default.
func defaultHarness(t *testing.T, appStore *store.Store, slug, digest string) *model.HarnessConfig {
	t.Helper()
	ctx := context.Background()
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: slug, Name: slug, BuiltIn: true,
		Image: "discobox-harness-" + slug + ":local", ImageDigest: digest,
		RunCommand: []string{slug}, Configured: true,
	}
	if err := appStore.CreateHarnessConfig(ctx, config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	project, err := appStore.GetProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	project.DefaultHarnessConfigID = config.ID
	project.JudgePoolID = "pool-1"
	if err := appStore.UpsertProject(ctx, project); err != nil {
		t.Fatalf("set project defaults: %v", err)
	}
	return config
}

// A project with a pool for its judge and a configured default harness has a
// judge: in that pool, running that harness, in judge mode, and Discobox's own.
func TestAProjectWithAHarnessGetsAJudge(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")

	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(sandboxes.created) != 1 {
		t.Fatalf("created %d discoboxes, want the judge", len(sandboxes.created))
	}
	judge := sandboxes.created[0]
	if judge.HarnessMode != sandboxconfig.HarnessModeJudge || judge.PoolID != "pool-1" {
		t.Fatalf("judge = %+v, want a discobox in judge mode in the judge's pool", judge)
	}

	// And a second reconcile leaves it alone rather than making another.
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(sandboxes.created) != 1 {
		t.Fatalf("created %d discoboxes, want the judge to be left alone", len(sandboxes.created))
	}
}

// The judge is Discobox's own, so it is not the project's work: it is not
// listed, it is not answered for by ID, and it does not keep a pool or a
// project from being deleted.
func TestAJudgeIsNotTheProjectsWork(t *testing.T) {
	ctx := context.Background()
	service, appStore, _ := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	project, err := appStore.GetProject(ctx, "project-1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if project.JudgeSandboxID == "" {
		t.Fatal("the project does not say which discobox is its judge")
	}
	judge, err := appStore.GetSandbox(ctx, "project-1", project.JudgeSandboxID)
	if err != nil {
		t.Fatalf("the judge is not there: %v", err)
	}

	listed, err := appStore.ListSandboxes(ctx, "project-1", "", nil)
	if err != nil {
		t.Fatalf("list sandboxes: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("listed %d discoboxes, want the judge left out", len(listed))
	}
	// Asked for, it is listed: what is left out by default is not hidden.
	withJudge, err := appStore.ListSandboxes(ctx, "project-1", "", nil, store.IncludingJudges())
	if err != nil {
		t.Fatalf("list sandboxes including judges: %v", err)
	}
	if len(withJudge) != 1 || withJudge[0].ID != judge.ID {
		t.Fatalf("listed %+v, want the judge when it is asked for", withJudge)
	}
	// What the delete gates ask: everything except this project's own judge.
	// The reconcilers still count every discobox, because a judge is as real
	// to a pool host as anything else it runs.
	for name, count := range map[string]func() (int64, error){
		"pool": func() (int64, error) { return appStore.CountWorkSandboxesForPool(ctx, "project-1", "pool-1") },
		"project": func() (int64, error) {
			return appStore.CountWorkSandboxesForProject(ctx, "project-1")
		},
	} {
		got, err := count()
		if err != nil {
			t.Fatalf("count %s sandboxes: %v", name, err)
		}
		if got != 0 {
			t.Fatalf("%s holds %d discoboxes, want the judge not counted: it is not work anybody would lose", name, got)
		}
	}
}

// Change what the project runs and the judge is replaced with it, because the
// harness is its image, its settings and the credential it answers with.
func TestAJudgeFollowsTheDefaultHarness(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	first := defaultHarness(t, appStore, "codex", "sha256:one")
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	original := sandboxes.created[0].ID

	// A different harness.
	second := defaultHarness(t, appStore, "claude-code", "sha256:two")
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() after the default changed: %v", err)
	}
	if len(sandboxes.deleted) != 1 || sandboxes.deleted[0] != original {
		t.Fatalf("deleted = %v, want the judge that ran %s taken away", sandboxes.deleted, first.Slug)
	}
	if len(sandboxes.created) != 2 || *sandboxes.created[1].HarnessConfigID != second.ID {
		t.Fatalf("created = %+v, want a judge running the new default", sandboxes.created)
	}

	// The same harness, re-pinned to another image.
	second.ImageDigest = "sha256:three"
	if err := appStore.UpdateHarnessConfig(ctx, second); err != nil {
		t.Fatalf("re-pin harness: %v", err)
	}
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() after the image moved: %v", err)
	}
	if len(sandboxes.created) != 3 || sandboxes.created[2].ImageDigest != "sha256:three" {
		t.Fatalf("created = %+v, want a judge on the image the harness is now pinned to", sandboxes.created)
	}
}

// An archived judge is one that has been taken away, which is what deleting a
// discobox does: it must not go on matching, or the project has a judge that
// answers nothing and convergence that thinks it is done.
func TestAnArchivedJudgeIsReplaced(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	archived := sandboxes.created[0]
	if err := sandboxes.archive(ctx, "project-1", archived.ID); err != nil {
		t.Fatalf("archive the judge: %v", err)
	}

	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(sandboxes.created) != 2 {
		t.Fatalf("created %d judges, want one to replace the archived one", len(sandboxes.created))
	}
	project, err := appStore.GetProject(ctx, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	if project.JudgeSandboxID == archived.ID {
		t.Fatal("the project still points at the archived judge")
	}
}

// A project with no judge pool recorded chooses one: a project made through
// the API never went through bootstrap, and a database older than the field
// would otherwise never judge anything again.
func TestAJudgePoolIsChosenWhenTheProjectHasNone(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	project, err := appStore.GetProject(ctx, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	project.JudgePoolID = ""
	project.DefaultPoolID = "pool-1"
	if err := appStore.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(sandboxes.created) != 1 || sandboxes.created[0].PoolID != "pool-1" {
		t.Fatalf("created = %+v, want a judge in the pool it chose", sandboxes.created)
	}
	project, err = appStore.GetProject(ctx, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	if project.JudgePoolID != "pool-1" {
		t.Fatalf("judge pool = %q, want the choice recorded so it does not move", project.JudgePoolID)
	}
}

// A pool on its way out is no place for a judge: the delete gate lets the pool
// go because a judge is not the project's work, and the pool's own convergence
// then counts every discobox on it and refuses forever. So the judge moves.
func TestAJudgeLeavesAPoolThatIsBeingDeleted(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	first := sandboxes.created[0]

	// Somewhere else to run, and the pool it is in on its way out.
	if err := appStore.CreatePool(ctx, &model.Pool{
		ID: "pool-2", ProjectID: "project-1",
		PoolManifest: model.PoolManifest{Name: "Second", ProviderInstanceID: "provider-1"},
	}); err != nil {
		t.Fatalf("create second pool: %v", err)
	}
	dying, err := appStore.GetPool(ctx, "project-1", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	previous := dying.Generation
	dying.IncrementGeneration()
	dying.RecordIntent(model.DesiredStateDeleted)
	if err := appStore.UpdatePoolWithGeneration(ctx, dying, previous); err != nil {
		t.Fatalf("record the pool's delete intent: %v", err)
	}
	project, err := appStore.GetProject(ctx, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	project.DefaultPoolID = "pool-2"
	if err := appStore.UpsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(sandboxes.created) != 2 || sandboxes.created[1].PoolID != "pool-2" {
		t.Fatalf("created = %+v, want the judge made again in the pool that is staying", sandboxes.created)
	}
	// Purged rather than archived: an archived discobox keeps its pool, and
	// the pool being deleted would never converge.
	if _, err := appStore.GetSandbox(ctx, "project-1", first.ID); err == nil {
		t.Fatal("the judge is still on the dying pool: an archived row keeps its pool and holds the delete open")
	}
}

// A judge that failed to come up is replaced rather than left in place: a
// project with one that cannot answer is a project that never gets a verdict.
func TestAFailedJudgeIsReplaced(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	failed := sandboxes.created[0]
	failed.State = model.SandboxStateFailed
	refused := "the pool refused it"
	failed.ErrorMessage = &refused
	if err := appStore.UpdateSandbox(ctx, failed); err != nil {
		t.Fatalf("mark the judge failed: %v", err)
	}

	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(sandboxes.deleted) != 1 || sandboxes.deleted[0] != failed.ID {
		t.Fatalf("deleted = %v, want the failed judge taken away", sandboxes.deleted)
	}
	if len(sandboxes.created) != 2 {
		t.Fatalf("created %d judges, want one to replace it", len(sandboxes.created))
	}
}

// A project that cannot judge has no judge, and one it had is taken away rather
// than left answering from a harness the project no longer uses.
func TestAProjectThatCannotJudgeHasNoJudge(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		brk  func(t *testing.T, appStore *store.Store)
	}{
		{"no default harness", func(t *testing.T, appStore *store.Store) {
			project, _ := appStore.GetProject(ctx, "project-1")
			project.DefaultHarnessConfigID = ""
			if err := appStore.UpsertProject(ctx, project); err != nil {
				t.Fatal(err)
			}
		}},
		{"a default that is not configured", func(t *testing.T, appStore *store.Store) {
			project, _ := appStore.GetProject(ctx, "project-1")
			config, _ := appStore.GetHarnessConfig(ctx, "project-1", project.DefaultHarnessConfigID)
			config.Configured = false
			if err := appStore.UpdateHarnessConfig(ctx, config); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, appStore, sandboxes := newJudgeTest(t)
			defaultHarness(t, appStore, "codex", "sha256:one")
			if _, err := service.Reconcile(ctx, "project-1"); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			judge := sandboxes.created[0].ID

			tc.brk(t, appStore)
			if _, err := service.Reconcile(ctx, "project-1"); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if len(sandboxes.deleted) != 1 || sandboxes.deleted[0] != judge {
				t.Fatalf("deleted = %v, want the judge taken away", sandboxes.deleted)
			}
			project, err := appStore.GetProject(ctx, "project-1")
			if err != nil {
				t.Fatal(err)
			}
			if project.JudgeSandboxID != "" {
				t.Fatalf("the project still points at %s as its judge", project.JudgeSandboxID)
			}
		})
	}
}

// A project with nowhere to run a judge has none. Nothing is removed here
// because nothing was ever made: the judge's pool is chosen from the project's
// own pools, and a project with none has no choice to make.
func TestAProjectWithNoPoolsHasNoJudge(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	if err := appStore.CreateProject(ctx, &model.Project{ID: "project-2", OwnerUserID: "user-1", Name: "Poolless"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := service.Reconcile(ctx, "project-2"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(sandboxes.created) != 0 {
		t.Fatalf("created %+v, want no judge where there is no pool to run one in", sandboxes.created)
	}
}

// A harness that runs no model cannot judge, and a project defaulting to one
// has no judge rather than one that refuses every ask.
func TestAShellHarnessIsNoJudge(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	defaultHarness(t, appStore, "shell", "sha256:shell")

	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(sandboxes.created) != 0 {
		t.Fatalf("created %+v, want no judge from a harness that runs no model", sandboxes.created)
	}
}

// Every project is looked at, so a judge converges even when whatever changed
// did not think to say so.
func TestScanNamesEveryProject(t *testing.T) {
	ctx := context.Background()
	service, appStore, _ := newJudgeTest(t)
	if err := appStore.CreateProject(ctx, &model.Project{ID: "project-2", OwnerUserID: "user-1", Name: "Another"}); err != nil {
		t.Fatalf("create second project: %v", err)
	}
	ids, err := service.ScanDirty(ctx)
	if err != nil {
		t.Fatalf("ScanDirty() error = %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("scanned %v, want every project", ids)
	}
}

// A judge that could not be brought up refuses with a sentence that says so
// and nothing else. Its own error names pool host paths, image references and
// whatever a provider's API said, and in unit 4 this string is what the
// discobox that asked is told (ADR 26-09-22-838 §4); the detail is the operator's, in
// the operator's log.
func TestAFailedJudgeRefusesWithoutQuotingItsOwnError(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	failed := sandboxes.created[0]
	failed.State = model.SandboxStateFailed
	said := "bind source path does not exist: /var/lib/discobox/pools/secret-path"
	failed.ErrorMessage = &said
	if err := appStore.UpdateSandbox(ctx, failed); err != nil {
		t.Fatalf("mark the judge failed: %v", err)
	}
	service.SetLeases(refusingLeases{})
	service.SetUses(approvedUses{})

	_, err := service.Judge(ctx, "pool-1", requestAsk())
	if err == nil {
		t.Fatal("a failed judge answered")
	}
	if strings.Contains(err.Error(), "secret-path") {
		t.Fatalf("error = %v, want the judge's own error kept off the wire", err)
	}
	if !strings.Contains(err.Error(), "could not be brought up") {
		t.Fatalf("error = %v, want it to say the judge could not be brought up", err)
	}
}

// refusingLeases stands in for the sandbox service's lease: reaching the judge
// is not what this test is about, and a failed judge is refused before it.
type refusingLeases struct{}

func (refusingLeases) AcquireSandboxHTTPClientForServer(context.Context, string, string, []string) (*services.HTTPClientLease, *model.Sandbox, error) {
	return nil, nil, errors.New("the judge should not have been reached")
}

// approvedUses stands in for the credential broker: what a use approves is its
// answer, and the tests that care about the answer say what it is.
type approvedUses struct {
	use services.ApprovedUse
	err error
	// asked counts the calls, because the use is resolved once to build the
	// question and once after the verdict.
	asked *int
}

func (a approvedUses) ApprovedUse(context.Context, string, string, string, string) (services.ApprovedUse, error) {
	if a.asked != nil {
		*a.asked++
	}
	if a.err != nil {
		return services.ApprovedUse{}, a.err
	}
	if a.use.Purpose == "" {
		return services.ApprovedUse{Purpose: "open a pull request in org/repo", Host: "api.github.com", Credential: "GitHub token"}, nil
	}
	return a.use, nil
}

// requestAsk is a pool asking about an ordinary observed request.
func requestAsk() services.JudgeAsk {
	return services.JudgeAsk{
		SandboxID: "sandbox-1", UseID: "use_abc", Round: 1,
		Request: &judge.Request{Method: http.MethodPost, URL: "https://api.github.com/repos/org/repo/pulls"},
	}
}

// Judging is a server's decision and it is off until one is made. A server
// that has not opted in makes no judge for a project that could have one,
// which is the whole of what it costs such a server: nothing.
func TestAServerThatDoesNotJudgeMakesNoJudge(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	service.enabled = false
	defaultHarness(t, appStore, "codex", "sha256:one")

	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(sandboxes.created) != 0 {
		t.Fatalf("created %d discoboxes, want none on a server that does not judge", len(sandboxes.created))
	}
	project, err := appStore.GetProject(ctx, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	if project.JudgeSandboxID != "" {
		t.Fatalf("the project points at %s as its judge", project.JudgeSandboxID)
	}
}

// Turning it off takes the judge away, through the same convergence that takes
// one away when the project's default harness goes. A judge nobody asks is a
// discobox holding a pool open for nothing.
func TestTurningJudgingOffTakesTheJudgeAway(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(sandboxes.created) != 1 {
		t.Fatalf("created %d discoboxes, want the judge", len(sandboxes.created))
	}
	judge := sandboxes.created[0].ID

	service.enabled = false
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(sandboxes.deleted) != 1 || sandboxes.deleted[0] != judge {
		t.Fatalf("deleted = %v, want the judge taken away", sandboxes.deleted)
	}
	project, err := appStore.GetProject(ctx, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	if project.JudgeSandboxID != "" {
		t.Fatalf("the project still points at %s as its judge", project.JudgeSandboxID)
	}
}

// And a pool that asks anyway is answered without a judge being looked for. It
// is no verdict, which is a refusal — the pool is told not to ask, so one that
// does is asking a question this server does not answer.
func TestAServerThatDoesNotJudgeRefusesToBeAsked(t *testing.T) {
	ctx := context.Background()
	service, appStore, _ := newJudgeTest(t)
	service.enabled = false
	defaultHarness(t, appStore, "codex", "sha256:one")

	_, err := service.Judge(ctx, "pool-1", requestAsk())
	if err == nil {
		t.Fatal("Judge() allowed a request on a server that does not judge")
	}
	if !strings.Contains(err.Error(), "does not judge") {
		t.Fatalf("Judge() error = %v, want it to say this server does not judge", err)
	}
}

// answeringJudge stands in for the judge's own agent, recording the job it was
// given and answering with what the test set.
type answeringJudge struct {
	server *httptest.Server
	mu     sync.Mutex
	jobs   []sandboxapi.JudgeJob
	answer sandboxapi.JudgeAnswer
	// delay is how long it thinks before answering.
	delay time.Duration
}

func newAnsweringJudge(t *testing.T, answer sandboxapi.JudgeAnswer) *answeringJudge {
	t.Helper()
	fake := &answeringJudge{answer: answer}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(fake.delay)
		var job sandboxapi.JudgeJob
		if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fake.mu.Lock()
		fake.jobs = append(fake.jobs, job)
		fake.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// Through the pointer: by value, an unset optional field marshals to
		// nothing and takes the whole encode with it.
		answer := fake.answer
		_ = json.NewEncoder(w).Encode(&answer)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *answeringJudge) asked() []sandboxapi.JudgeJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sandboxapi.JudgeJob(nil), f.jobs...)
}

// AcquireSandboxHTTPClientForServer hands out a lease pointed at the fake.
func (f *answeringJudge) AcquireSandboxHTTPClientForServer(_ context.Context, projectID, sandboxID string, _ []string) (*services.HTTPClientLease, *model.Sandbox, error) {
	lease := transport.NewHTTPClientLeaseWithBaseURL(f.server.Client(), f.server.URL, func() {})
	return lease, &model.Sandbox{ID: sandboxID, ProjectID: projectID, PoolID: "pool-1"}, nil
}

// The question a judge is put is the control plane's, not the asking pool's.
// The pool says which discobox is spending which use; the sentence that use
// approves, the credential behind it and the host it is approved for are read
// from the live grant, so nothing a pool sends can widen its own question.
func TestTheQuestionIsReadFromTheGrantAndNotFromTheAsk(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	ready(t, appStore, sandboxes.created[0])

	fake := newAnsweringJudge(t, sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(true), Reason: "that is the approved use"})
	service.SetLeases(fake)
	asked := 0
	service.SetUses(approvedUses{asked: &asked, use: services.ApprovedUse{
		Purpose: "open a pull request in org/repo", Host: "github.com", Credential: "GitHub token",
	}})

	answer, err := service.Judge(ctx, "pool-1", requestAsk())
	if err != nil {
		t.Fatalf("Judge() error = %v", err)
	}
	if !answer.Allow {
		t.Fatalf("answer = %+v, want the judge's allow", answer)
	}
	jobs := fake.asked()
	if len(jobs) != 1 {
		t.Fatalf("the judge was asked %d times, want once", len(jobs))
	}
	if jobs[0].Purpose != "open a pull request in org/repo" || jobs[0].Host != "github.com" {
		t.Fatalf("job = %+v, want the sentence and host from the grant", jobs[0])
	}
	if jobs[0].Credential.Or("") != "GitHub token" {
		t.Fatalf("job named the credential %q, want the words a person reads", jobs[0].Credential.Or(""))
	}
	// Once to build the question, once after the verdict.
	if asked != 2 {
		t.Fatalf("the use was resolved %d times, want it asked again after the verdict", asked)
	}
}

// A grant revoked while the judge was thinking is a request that is not
// allowed, whatever the judge said. The verdict was about a use that no longer
// exists (ADR 26-09-22-838 §4).
func TestAUseRevokedWhileTheJudgeThoughtIsNotAllowed(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	ready(t, appStore, sandboxes.created[0])

	fake := newAnsweringJudge(t, sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(true), Reason: "that is the approved use"})
	service.SetLeases(fake)
	service.SetUses(&revokedAfterFirst{})

	_, err := service.Judge(ctx, "pool-1", requestAsk())
	if err == nil {
		t.Fatal("Judge() allowed a request whose use was revoked while it was being judged")
	}
	if !strings.Contains(err.Error(), "no live approved use") {
		t.Fatalf("Judge() error = %v, want it to say the use is gone", err)
	}
	// And the judge's allow is not on record: it is not what the request was
	// refused on, and the proxy's blocked row already says what was.
	verdicts, err := appStore.ListCredentialVerdicts(ctx, "project-1", store.CredentialVerdictFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 0 {
		t.Fatalf("recorded %+v for a request refused on a revoked use", verdicts)
	}
}

// revokedAfterFirst answers once and is gone by the time it is asked again.
type revokedAfterFirst struct{ asked int }

func (r *revokedAfterFirst) ApprovedUse(context.Context, string, string, string, string) (services.ApprovedUse, error) {
	r.asked++
	if r.asked > 1 {
		return services.ApprovedUse{}, apperrors.NewStatusError(http.StatusForbidden, "no live approved use by that ID")
	}
	return services.ApprovedUse{Purpose: "open a pull request in org/repo", Host: "github.com", Credential: "GitHub token"}, nil
}

// ready marks the judge as up, since a judge that could not be brought up is
// refused before anything is asked of it.
func ready(t *testing.T, appStore *store.Store, judge *model.Sandbox) {
	t.Helper()
	judge.State = model.SandboxStateReady
	if err := appStore.UpdateSandbox(context.Background(), judge); err != nil {
		t.Fatalf("mark the judge running: %v", err)
	}
}

// The rules about the evidence are the trusted side's, and they are checked
// once the question is composed. A first ask that carries the body has skipped
// the step the rounds exist for: the judge asks to be shown it.
func TestAFirstAskCarryingTheBodyIsRefused(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	ready(t, appStore, sandboxes.created[0])
	fake := newAnsweringJudge(t, sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(true)})
	service.SetLeases(fake)
	service.SetUses(approvedUses{})

	ask := requestAsk()
	ask.Request.Body = &judge.Body{MediaType: "application/json", Length: 2, Form: judge.FormJSON, Content: "{}"}
	_, err := service.Judge(ctx, "pool-1", ask)
	if err == nil {
		t.Fatal("a first ask carrying the body was judged")
	}
	if len(fake.asked()) != 0 {
		t.Fatalf("the judge was asked %d times, want a malformed ask refused before it", len(fake.asked()))
	}
}

// Every answer the judge gives is recorded before it goes back to the pool,
// whichever way it went: a request that carried a credential has a verdict on
// record, and it says which judge gave it and how long the round trip took
// (ADR 26-09-22-838 §8).
func TestEveryAnswerIsRecordedBeforeItGoesBack(t *testing.T) {
	cases := []struct {
		name   string
		answer sandboxapi.JudgeAnswer
		allow  bool
		need   *judge.Need
	}{
		{"allow", sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(true), Reason: "that is the approved use"}, true, nil},
		{"deny", sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(false), Reason: "deleting a repository is not opening a pull request"}, false, nil},
		{"need", sandboxapi.JudgeAnswer{
			Need:   sandboxapi.NewOptJudgeNeed(sandboxapi.JudgeNeed{Body: sandboxapi.JudgeNeedBodyJSON}),
			Reason: "the operation is in the body",
		}, false, &judge.Need{Body: judge.FormJSON}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, appStore, sandboxes := newJudgeTest(t)
			config := defaultHarness(t, appStore, "codex", "sha256:one")
			if _, err := service.Reconcile(ctx, "project-1"); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			judgeSandbox := sandboxes.created[0]
			ready(t, appStore, judgeSandbox)
			fake := newAnsweringJudge(t, tc.answer)
			fake.delay = 20 * time.Millisecond
			service.SetLeases(fake)
			service.SetUses(approvedUses{use: services.ApprovedUse{
				Purpose: "open a pull request in org/repo", Host: "api.github.com", Credential: "GitHub token", GrantID: "grant-1",
			}})

			ask := requestAsk()
			ask.Command = []string{"gh", "pr", "create"}
			if _, err := service.Judge(ctx, "pool-1", ask); err != nil {
				t.Fatalf("Judge() error = %v", err)
			}

			verdicts, err := appStore.ListCredentialVerdicts(ctx, "project-1", store.CredentialVerdictFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if len(verdicts) != 1 {
				t.Fatalf("recorded %d verdicts, want the one answer", len(verdicts))
			}
			v := verdicts[0]
			if v.Kind != model.CredentialVerdictKindRequest || v.Origin != model.CredentialVerdictOriginJudge {
				t.Fatalf("kind, origin = %q, %q, want a request verdict from the judge", v.Kind, v.Origin)
			}
			if v.SandboxID != "sandbox-1" || v.UseID != "use_abc" || v.GrantID != "grant-1" {
				t.Fatalf("verdict = %+v, want it against the asking discobox's use and its grant", v)
			}
			if v.Allow != tc.allow || v.Reason != tc.answer.Reason {
				t.Fatalf("allow, reason = %v, %q, want the judge's answer", v.Allow, v.Reason)
			}
			if (v.Need == nil) != (tc.need == nil) || (v.Need != nil && *v.Need != *tc.need) {
				t.Fatalf("need = %+v, want %+v", v.Need, tc.need)
			}
			if v.Request == nil || v.Request.Method != http.MethodPost || v.Request.URL != ask.Request.URL || v.Round != 1 {
				t.Fatalf("request, round = %+v, %d, want the evidence the judge was shown", v.Request, v.Round)
			}
			if len(v.Command) != 3 || v.Command[0] != "gh" {
				t.Fatalf("command = %v, want the declared command kept as context", v.Command)
			}
			if v.Role != judge.Role || v.PromptVersion != judge.PromptVersion || !strings.Contains(v.Prompt, "open a pull request in org/repo") {
				t.Fatalf("role, version, prompt = %q, %q, %q, want the question as it was put", v.Role, v.PromptVersion, v.Prompt)
			}
			if v.JudgeSandboxID != judgeSandbox.ID || v.HarnessConfigID != config.ID || v.Image != config.Image || v.ImageDigest != "sha256:one" {
				t.Fatalf("judge = %q %q %q %q, want the discobox that answered and what it ran",
					v.JudgeSandboxID, v.HarnessConfigID, v.Image, v.ImageDigest)
			}
			if v.LatencyMS < fake.delay.Milliseconds() {
				t.Fatalf("latency = %dms, want the round trip, at least the %s the judge thought", v.LatencyMS, fake.delay)
			}
		})
	}
}

// An answer that cannot be recorded is no verdict. A credential must not go
// out on a decision nobody can read back (ADR 26-09-22-838 §4).
func TestAnAnswerThatCannotBeRecordedIsNoVerdict(t *testing.T) {
	ctx := context.Background()
	service, appStore, sandboxes := newJudgeTest(t)
	defaultHarness(t, appStore, "codex", "sha256:one")
	if _, err := service.Reconcile(ctx, "project-1"); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	ready(t, appStore, sandboxes.created[0])
	service.SetLeases(newAnsweringJudge(t, sandboxapi.JudgeAnswer{Allow: sandboxapi.NewOptBool(true), Reason: "that is the approved use"}))
	service.SetUses(approvedUses{})
	if err := appStore.Transaction(ctx, func(_ *store.Store, tx *gorm.DB) error {
		return tx.Exec("DROP TABLE credential_verdicts").Error
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Judge(ctx, "pool-1", requestAsk()); err == nil {
		t.Fatal("Judge() answered with a verdict it could not record")
	}
}
