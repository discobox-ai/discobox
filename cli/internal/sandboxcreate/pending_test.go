package sandboxcreate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/discobox-ai/discobox/api/gen"
	model "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/internal/hostid"
	"github.com/discobox-ai/discobox/internal/privatefs"
)

func pendingTestHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME"} {
		t.Setenv(key, home)
	}
	t.Setenv(hostid.EnvVar, "host-1")
}
func pendingFixture(t *testing.T, empty, bind bool) (*LocalSources, *model.Sandbox, string) {
	t.Helper()
	dir := t.TempDir()
	if !empty {
		if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("original bytes\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	body, local, err := BuildPromptSandboxBody(t.Context(), PromptOptions{Platform: linuxSandbox, Source: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(local.Close)
	sb := &model.Sandbox{ID: "sbx_1", ProjectId: "project-1", Origin: body.Origin, Runtime: model.SandboxRuntime{State: api.SandboxRuntimeStateAwaitingSource}}
	source := body.Config.Source.Value
	source.Delivery = api.NewOptGitSourceDelivery(api.GitSourceDeliveryPush)
	source.Slug = api.NewOptString("primary")
	sb.Config.Source = api.NewOptGitSource(source)
	if err := local.stagePending(t.Context(), "https://user:secret@example.test", "project-1", body); err != nil {
		t.Fatal(err)
	}
	if bind {
		if err := local.bindPending(t.Context(), sb); err != nil {
			t.Fatal(err)
		}
	}
	return local, sb, dir
}
func TestPendingSourceSurvivesDirectoryChangesAndInterruptedBinding(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "files", true: "empty"}[empty], func(t *testing.T) {
			pendingTestHome(t)
			local, sb, dir := pendingFixture(t, empty, false)
			saved := local.sources[0].pending
			local.Close()
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
			retry := NewLocalSources(nil)
			defer retry.Close()
			repo, err := retry.RecoverSource(t.Context(), "https://example.test", sb, PendingSourcePushes(sb)[0])
			if err != nil {
				t.Fatal(err)
			}
			m, err := readManifest(saved)
			if err != nil {
				t.Fatal(err)
			}
			if m.Sandbox != sb.ID {
				t.Fatalf("binding = %q", m.Sandbox)
			}
			if empty {
				if m.Snapshot != "" {
					t.Fatal("empty source has snapshot")
				}
			} else {
				got := runSourceTestGit(t, repo)("show", m.Snapshot+":notes.txt")
				if got != "original bytes\n" {
					t.Fatalf("snapshot = %q", got)
				}
			}
			if err := filepath.WalkDir(saved, func(path string, _ os.DirEntry, e error) error {
				if e != nil {
					return e
				}
				return privatefs.Check(path)
			}); err != nil {
				t.Fatal(err)
			}
			bytes, err := os.ReadFile(filepath.Join(saved, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(bytes), "secret") {
				t.Fatal("saved endpoint credential")
			}
		})
	}
}
func TestPendingSourceRefusesMismatchedOrMissingState(t *testing.T) {
	for _, kind := range []string{"endpoint", "project", "host", "sandbox", "directory", "slug", "pin", "snapshot", "missing", "ref moved", "corrupt", "ambiguous"} {
		t.Run(kind, func(t *testing.T) {
			pendingTestHome(t)
			local, sb, _ := pendingFixture(t, false, kind != "ambiguous")
			endpoint := "https://example.test"
			source := sb.Config.Source.Value
			saved := local.sources[0].pending
			switch kind {
			case "endpoint":
				endpoint = "https://alias.test"
			case "project":
				sb.ProjectId = "another-project"
			case "host":
				sb.Origin.Value.HostId = "another-host"
			case "sandbox":
				sb.ID = "another-sandbox"
			case "directory":
				source.LocalDirectory = api.NewOptString("/another/path")
			case "slug":
				source.Slug = api.NewOptString("another-slug")
			case "pin":
				source.Checkout.Value.Commit = api.NewOptString(strings.Repeat("a", 40))
			case "snapshot":
				source.Workspace.Value.SnapshotRef = api.NewOptString("refs/discobox/run/another")
			case "missing":
				if err := os.RemoveAll(saved); err != nil {
					t.Fatal(err)
				}
			case "ref moved":
				runSourceTestGit(t, local.sources[0].repoRoot)("update-ref", source.Workspace.Value.SnapshotRef.Value, source.Checkout.Value.Commit.Value)
			case "corrupt":
				if err := os.WriteFile(filepath.Join(saved, "manifest.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "ambiguous":
				// An identical staged manifest is sufficient to make lookup ambiguous;
				// recovery must refuse before choosing or validating either repository.
				other := filepath.Join(filepath.Dir(saved), "duplicate")
				if err := privateDirectory(other); err != nil {
					t.Fatal(err)
				}
				m, err := readManifest(saved)
				if err != nil {
					t.Fatal(err)
				}
				if err = writeManifest(other, m); err != nil {
					t.Fatal(err)
				}
			}
			sb.Config.Source = api.NewOptGitSource(source)
			retry := NewLocalSources(nil)
			defer retry.Close()
			if _, err := retry.RecoverSource(t.Context(), endpoint, sb, PendingSourcePushes(sb)[0]); err == nil {
				t.Fatal("accepted unavailable/mismatched state")
			}
		})
	}
}
func TestPendingSourceRetainsLostCompletionUntilConfirmedRead(t *testing.T) {
	pendingTestHome(t)
	local, sb, _ := pendingFixture(t, false, true)
	saved := local.sources[0].pending
	client := &awaitClient{runtimes: []model.SandboxRuntime{{State: api.SandboxRuntimeStateAwaitingSource}}, completeErr: errors.New("lost completion response")}
	err := DeliverSource(t.Context(), client, "project-1", sb, local, gitOriginServer(t), "token", nil, DeliveryOptions{})
	if err == nil {
		t.Fatal("expected lost response")
	}
	if _, err := os.Stat(saved); err != nil {
		t.Fatalf("lost response deleted snapshot: %v", err)
	}
	if err := CleanupConfirmedSources(t.Context(), "https://example.test", sb); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(saved); err != nil {
		t.Fatal("unconfirmed read deleted snapshot")
	}
	sb.Runtime.SourceDeliveredAt = api.NewOptDateTime(time.Now())
	if err := CleanupConfirmedSources(t.Context(), "https://example.test", sb); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(saved); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed snapshot still exists: %v", err)
	}
}
func TestPendingSourceCleanupCannotRaceDelivery(t *testing.T) {
	pendingTestHome(t)
	local, sb, _ := pendingFixture(t, false, true)
	saved := local.sources[0].pending
	retry := NewLocalSources(nil)
	if _, err := retry.RecoverSource(t.Context(), "https://example.test", sb, PendingSourcePushes(sb)[0]); err != nil {
		t.Fatal(err)
	}
	defer retry.Close()
	sb.Runtime.SourceDeliveredAt = api.NewOptDateTime(time.Now())
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if err := CleanupConfirmedSources(ctx, "https://example.test", sb); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup during delivery = %v", err)
	}
	if _, err := os.Stat(saved); err != nil {
		t.Fatal("cleanup removed active repository")
	}
	retry.Close()
	if err := CleanupConfirmedSources(t.Context(), "https://example.test", sb); err != nil {
		t.Fatal(err)
	}
}

func TestPendingSourcesPrecedeCreateAndCacheDeliveryPrecedesBoot(t *testing.T) {
	pendingTestHome(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("before boot\n"), 0600); err != nil {
		t.Fatal(err)
	}
	body, local, err := BuildPromptSandboxBody(t.Context(), PromptOptions{Platform: linuxSandbox, Source: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	pool := newFakePool(t)
	if err := local.stagePending(t.Context(), pool.server.URL, "p", body); err != nil {
		t.Fatal(err)
	}
	source := body.Config.Source.Value
	source.Slug = api.NewOptString("primary")
	source.Delivery = api.NewOptGitSourceDelivery(api.GitSourceDeliveryCache)
	sb := cacheSandbox("sbx-one", source)
	sb.ProjectId = "p"
	sb.Origin = body.Origin
	sb.Runtime.State = api.SandboxRuntimeStatePending
	if err := local.bindPending(t.Context(), sb); err != nil {
		t.Fatal(err)
	}
	saved := local.sources[0].pending
	if err := DeliverSource(t.Context(), pool, "p", sb, local, pool.server.URL, "token", nil, DeliveryOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(pool.completed["sbx-one"]) != 1 {
		t.Fatal("cache delivery did not complete while sandbox was pending")
	}
	if _, err := os.Stat(saved); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("confirmed snapshot remains: %v", err)
	}
}
func TestPendingSnapshotWriteFailurePreventsCreate(t *testing.T) {
	pendingTestHome(t)
	root, err := pendingRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	creator := &fakeCreator{}
	_, _, err = CreatePromptSandbox(t.Context(), creator, "project-1", "https://example.test", PromptOptions{Platform: linuxSandbox, Source: t.TempDir()}, nil)
	if err == nil {
		t.Fatal("create accepted unavailable recovery storage")
	}
	if len(creator.names) != 0 {
		t.Fatal("create sent before recovery snapshot was published")
	}
}
func TestPartialMultiSourceDeliveryRetainsWholePendingSet(t *testing.T) {
	pendingTestHome(t)
	primary, extra := t.TempDir(), t.TempDir()
	for _, dir := range []string{primary, extra} {
		if err := os.WriteFile(filepath.Join(dir, "file"), []byte(dir), 0600); err != nil {
			t.Fatal(err)
		}
	}
	body, local, err := BuildPromptSandboxBody(t.Context(), PromptOptions{Platform: linuxSandbox, Source: primary, Include: []string{extra}})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	source := body.Config.Source.Value
	source.Slug = api.NewOptString("primary")
	source.Delivery = api.NewOptGitSourceDelivery(api.GitSourceDeliveryPush)
	sb := parkedWithPushSource(source.Checkout.Value.Commit.Value)
	sb.Origin = body.Origin
	sb.Config.Source = api.NewOptGitSource(source)
	refs := api.SandboxConfigSourceCodeReferences{}
	for key, ref := range body.Config.SourceCodeReferences.Value {
		ref.Delivery = api.NewOptGitSourceDelivery(api.GitSourceDeliveryPush)
		refs[key] = ref
	}
	sb.Config.SourceCodeReferences = api.NewOptSandboxConfigSourceCodeReferences(refs)
	if err := local.stagePending(t.Context(), "https://example.test", "project-1", body); err != nil {
		t.Fatal(err)
	}
	if err := local.bindPending(t.Context(), sb); err != nil {
		t.Fatal(err)
	}
	var saved []string
	for _, entry := range local.sources {
		saved = append(saved, entry.pending)
	}
	client := &awaitClient{runtimes: []model.SandboxRuntime{{State: api.SandboxRuntimeStateAwaitingSource}}, completeOK: true}
	// This origin serves only primary. It accepts the first source and refuses
	// the second; neither repository may be discarded after partial delivery.
	if err := DeliverSource(t.Context(), client, "project-1", sb, local, gitOriginServer(t), "token", nil, DeliveryOptions{}); err == nil {
		t.Fatal("expected missing extra-source origin")
	}
	if client.completes != 0 {
		t.Fatal("reported incomplete aggregate delivery")
	}
	for _, path := range saved {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("partial delivery discarded %s: %v", path, err)
		}
	}
	retry := NewLocalSources(nil)
	defer retry.Close()
	for _, entry := range PendingSourcePushes(sb) {
		if _, err := retry.RecoverSource(t.Context(), "https://example.test", sb, entry); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCorruptAbandonedSnapshotDoesNotBlockUnrelatedConfirmedBox(t *testing.T) {
	pendingTestHome(t)
	local, sb, _ := pendingFixture(t, false, true)
	saved := local.sources[0].pending
	if err := os.WriteFile(filepath.Join(saved, "manifest.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	sb.ID = "unrelated-box"
	sb.Runtime.SourceDeliveredAt = api.NewOptDateTime(time.Now())
	if err := CleanupConfirmedSources(t.Context(), "https://example.test", sb); err != nil {
		t.Fatalf("unrelated confirmed box blocked: %v", err)
	}
	if _, err := os.Stat(saved); err != nil {
		t.Fatal("corrupt state was removed")
	}
}
func TestDirectoryCaptureAndMeasurementExcludeRetainedSnapshots(t *testing.T) {
	pendingTestHome(t)
	local, _, _ := pendingFixture(t, false, true)
	saved := local.sources[0].pending
	root, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	// A source cannot negate the exclusion of another source's private bytes.
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("!**\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveRunSource(t.Context(), root, runSourceOptions{Platform: linuxSandbox, IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatal(err)
	}
	defer resolved.close()
	tree := runSourceTestGit(t, resolved.RepoRoot)("ls-tree", "-r", "--name-only", resolved.Workspace.SnapshotRef)
	if strings.Contains(tree, "pending-sources") || strings.Contains(tree, "manifest.json") {
		t.Fatalf("exported recovery data: %s", tree)
	}
	if !strings.Contains(tree, "keep.txt") {
		t.Fatalf("source omitted ordinary files: %s", tree)
	}
	walk := MeasureDirectory(t.Context(), root)
	defer walk.Stop()
	deadline := time.After(3 * time.Second)
	for !walk.Total().Done {
		select {
		case <-deadline:
			t.Fatal("measurement did not finish")
		case <-time.After(time.Millisecond):
		}
	}
	if got := walk.Total().Files; got != 2 {
		t.Fatalf("measured %d files, want keep.txt and .gitignore only", got)
	}
	// Selecting the store itself carries no files either.
	resolved, err = resolveRunSource(t.Context(), saved, runSourceOptions{Platform: linuxSandbox, IncludeDirty: IncludeDirtyAuto})
	if err != nil {
		t.Fatal(err)
	}
	defer resolved.close()
	if resolved.Workspace.SnapshotRef != "" {
		t.Fatal("direct store capture carried private files")
	}
}

func TestGitWorkspaceCapturesExcludeRetainedSnapshotsWithoutChangingIndex(t *testing.T) {
	for _, unborn := range []bool{false, true} {
		t.Run(map[bool]string{false: "git", true: "unborn"}[unborn], func(t *testing.T) {
			pendingTestHome(t)
			pendingFixture(t, false, true)
			root, err := os.UserConfigDir()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("keep\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("!**\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git := runSourceTestGit(t, root)
			git("init", "-b", "main")
			git("config", "user.name", "Test")
			git("config", "user.email", "test@example.test")
			git("add", "keep.txt")
			if !unborn {
				git("commit", "-m", "base")
				if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("edited\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			indexBefore := git("write-tree")
			source, err := resolveRunSource(t.Context(), root, runSourceOptions{Platform: linuxSandbox, IncludeDirty: IncludeDirtyAlways})
			if err != nil {
				t.Fatal(err)
			}
			defer source.close()
			tree := git("ls-tree", "-r", "--name-only", source.Workspace.SnapshotRef)
			if strings.Contains(tree, "pending-sources") || strings.Contains(tree, "manifest.json") {
				t.Fatalf("exported retained snapshot: %s", tree)
			}
			if !strings.Contains(tree, "keep.txt") {
				t.Fatalf("missing ordinary workspace: %s", tree)
			}
			if after := git("write-tree"); after != indexBefore {
				t.Fatalf("changed person's index: %s -> %s", indexBefore, after)
			}
		})
	}
}

type lostCreateResponse struct {
	fakeCreator
	sandbox *model.Sandbox
	project string
}

func (f *lostCreateResponse) CreateSandbox(_ context.Context, body *model.CreateSandboxBody, params api.CreateSandboxParams) (api.CreateSandboxRes, error) {
	f.project = params.ProjectId
	f.sandbox = &model.Sandbox{ID: "sbx_1", ProjectId: params.ProjectId, Origin: body.Origin, Runtime: model.SandboxRuntime{State: api.SandboxRuntimeStateAwaitingSource}}
	source := body.Config.Source.Value
	source.Slug = api.NewOptString("primary")
	source.Delivery = api.NewOptGitSourceDelivery(api.GitSourceDeliveryPush)
	f.sandbox.Config.Source = api.NewOptGitSource(source)
	return nil, errors.New("create response lost")
}
func TestAmbiguousCreateRetainsSnapshotWithCanonicalProject(t *testing.T) {
	pendingTestHome(t)
	creator := &lostCreateResponse{}
	sb, local, err := CreatePromptSandbox(t.Context(), creator, "default", "https://example.test", PromptOptions{Platform: linuxSandbox, Source: t.TempDir()}, nil)
	if err == nil || sb != nil || local != nil {
		t.Fatalf("ambiguous create = %v %v %v", sb, local, err)
	}
	if !strings.Contains(err.Error(), "snapshots retained at") {
		t.Fatal("ambiguous create hid retained state")
	}
	if creator.project != "project-1" {
		t.Fatalf("create targeted mutable selector %s", creator.project)
	}
	retry := NewLocalSources(nil)
	defer retry.Close()
	if _, err := retry.RecoverSource(t.Context(), "https://example.test", creator.sandbox, PendingSourcePushes(creator.sandbox)[0]); err != nil {
		t.Fatalf("recover ambiguous create: %v", err)
	}
}
