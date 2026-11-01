package sandboxes

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/sandboxmeta"
	"github.com/discobox-ai/discobox/pkg/tarsums"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/sandboxexport"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// treeProvider records what an import handed the pool, and when: the ordering
// is the decision an import rests on (ADR 0123 §3), so the test can see it.
type treeProvider struct {
	*recordingProvider
	imported     []byte
	importedPool string
	// sandboxesAtImport is how many sandbox rows existed when the tree landed.
	// It must be zero: a row is what wakes the reconciler, and a reconciler that
	// runs before the tree arrives builds a container around an empty workspace.
	sandboxesAtImport int
	countRows         func() int
	exportTree        []byte
	// exportedPool is the pool ExportSandbox addressed. It comes off the row
	// rather than out of runtime state, which is what lets a discobox whose
	// create never reached an agent still be exported.
	exportedPool string
	// exported is what ExportSandbox asked the tree to be read with.
	exported sandbox.Delivery
}

func (p *treeProvider) ExportTree(_ context.Context, _ sandbox.SandboxRef, poolID string, delivered sandbox.Delivery, _ []byte) (io.ReadCloser, error) {
	p.exportedPool = poolID
	p.exported = delivered
	return io.NopCloser(bytes.NewReader(p.exportTree)), nil
}

func (p *treeProvider) ImportTree(_ context.Context, _ sandbox.SandboxRef, poolID string, _ platform.Platform, tree io.Reader) (string, error) {
	if p.countRows != nil {
		p.sandboxesAtImport = p.countRows()
	}
	data, err := io.ReadAll(tree)
	if err != nil {
		return "", err
	}
	p.imported = data
	p.importedPool = poolID
	return poolID, nil
}

func transferFixture(t *testing.T) (context.Context, *Service, *store.Store, *treeProvider) {
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
	engine, err := reconcile.New(db.Write, reconcile.Options{SingleNode: true})
	if err != nil {
		t.Fatalf("create reconcile engine: %v", err)
	}
	if err := db.Write.WithContext(ctx).Create(&model.Project{
		ID: "project-1", OwnerUserID: "user-1", Name: "Project", DefaultPoolID: "pool-1",
	}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	instance := &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "test", Name: "Test"}
	if err := appStore.CreateSandboxProviderInstance(ctx, instance); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if err := appStore.CreatePool(ctx, &model.Pool{
		ID: "pool-1", ProjectID: "project-1",
		PoolManifest: model.PoolManifest{Name: "pool-1", ProviderInstanceID: instance.ID},
	}); err != nil {
		t.Fatalf("create pool: %v", err)
	}
	provider := &treeProvider{recordingProvider: &recordingProvider{}}
	provider.countRows = func() int {
		sandboxes, err := appStore.ListSandboxes(ctx, "project-1", "", nil)
		if err != nil {
			return -1
		}
		return len(sandboxes)
	}
	manager := sandbox.NewProviderManager(appStore)
	manager.RegisterProvider("test", provider)
	manager.SetDefault("test")
	svc := NewService(appStore, manager, "user-1", engine)
	// A box on a harness bundle applies it onto the release's platform, which
	// a create pins beside it.
	svc.SetReleaseBundles(release(platformOld))
	return ctx, svc, appStore, provider
}

// configuredHarness stores a configured harness config on its bundle, the
// codex one pinned to codexBundleV1.
func configuredHarness(t *testing.T, st *store.Store, slug, name string) *model.HarnessConfig {
	t.Helper()
	digest := codexBundleV1
	if slug != "codex" {
		sum := sha256.Sum256([]byte(slug))
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	config := &model.HarnessConfig{
		ProjectID: "project-1", Slug: slug, Name: name, Configured: true,
		Bundle: "ghcr.io/discobox-ai/discobox-harness-" + slug + ":v1", BundleDigest: digest,
		RunCommand: []string{slug},
	}
	if err := st.CreateHarnessConfig(context.Background(), config); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	return config
}

// exportArchive builds a .dbox the way ExportSandbox would.
func exportArchive(t *testing.T, mutate func(*sandboxexport.Manifest), files map[string]string) []byte {
	t.Helper()
	var tree bytes.Buffer
	writer := tarsums.NewWriter(&tree)
	for name, content := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := &sandboxexport.Manifest{
		From: sandboxexport.Source{ProjectID: "elsewhere", SandboxID: "sbx_old"},
		Sandbox: sandboxexport.Spec{
			Name:    "my-box",
			Harness: sandboxexport.Harness{Slug: "codex", Name: "Codex"},
			Origin:  &model.Origin{HostID: "host-abc", Hostname: "laptop", User: "dev"},
			Manifest: model.SandboxManifest{
				Bundle: codexBundle, BundleDigest: codexBundleV2, HarnessMode: "run",
			},
		},
	}
	if mutate != nil {
		mutate(manifest)
	}
	var out bytes.Buffer
	if err := sandboxexport.Write(&out, manifest, bytes.NewReader(tree.Bytes())); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestImportRestoresTheTreeBeforeTheRowExists(t *testing.T) {
	ctx, svc, st, provider := transferFixture(t)
	config := configuredHarness(t, st, "codex", "Codex")
	// An archive from before bundles, of a discobox on a runnable image.
	archive := exportArchive(t, func(m *sandboxexport.Manifest) {
		m.Sandbox.Manifest.Bundle, m.Sandbox.Manifest.BundleDigest = "", ""
		m.Sandbox.Manifest.Image, m.Sandbox.Manifest.ImageDigest = legacyImage, legacyImageDigest
	}, map[string]string{"data/.bashrc": "x\n"})

	result, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(archive), services.SandboxImportOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if provider.sandboxesAtImport != 0 {
		t.Errorf("%d sandbox rows existed when the tree landed; the row is what wakes the reconciler, so it must come second",
			provider.sandboxesAtImport)
	}
	if provider.importedPool != "pool-1" {
		t.Errorf("tree landed on pool %q, want the project default", provider.importedPool)
	}
	// The pool agent restores relative names; the tree/ prefix is the archive's.
	if names := tarNames(t, provider.imported); len(names) != 1 || names[0] != "data/.bashrc" {
		t.Errorf("pool agent received %v, want [data/.bashrc]", names)
	}
	sb := result.Sandbox
	if sb.Name != "my-box" || sb.PoolID != "pool-1" {
		t.Fatalf("sandbox = %+v", sb)
	}
	// The bundles come from the destination's harness config and release,
	// as a create's do. They cannot come from the archive: everything else
	// the harness contributes to the container is read off this row, and the
	// archive's runnable image is one no harness is any more.
	if harness := sb.HarnessPin(); harness.Reference != config.Bundle || harness.Digest != codexBundleV1 || sb.PlatformPin().Digest != platformOld || sb.AgentBundleDigest != agentV1 {
		t.Errorf("pins = %+v, platform %q, agent %q, want the destination harness config's and release's", harness, sb.PlatformPin().Digest, sb.AgentBundleDigest)
	}
	if sb.Image != "" || sb.ImageDigest != "" {
		t.Errorf("image = %q@%q, want the archive's image left behind", sb.Image, sb.ImageDigest)
	}
	if sb.Origin == nil || sb.Origin.HostID != "host-abc" {
		t.Fatalf("origin = %+v; without it the discobox has no source data mount and no origin key", sb.Origin)
	}
	if sb.OriginKey == nil || *sb.OriginKey == "" {
		t.Error("originKey is unset, so `discobox ls` in the repository would not list the imported discobox")
	}
}

// The bundle and the rest of what a harness contributes have to come from the
// same place. RunCommand, Files and Volumes are read off the destination
// harness config row when the container is built, so a bundle from the
// archive and a command from the destination is a container that starts and
// a harness that does not.
func TestImportPinsTheBundleToTheHarnessItWillRun(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	claude := configuredHarness(t, st, "claude", "Claude Code")
	archive := exportArchive(t, func(m *sandboxexport.Manifest) {
		m.Sandbox.Harness = sandboxexport.Harness{Slug: "codex", Name: "Codex"}
	}, nil)

	result, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(archive),
		services.SandboxImportOptions{Harness: "claude"})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	sb := result.Sandbox
	if sb.HarnessConfigID == nil || *sb.HarnessConfigID != claude.ID {
		t.Fatalf("harness config = %v, want the one --harness named", sb.HarnessConfigID)
	}
	if harness := sb.HarnessPin(); harness.Reference != claude.Bundle || harness.Digest != claude.BundleDigest {
		t.Errorf("bundle = %q@%q, want %q@%q: --harness moved the harness but not the bundle it applies",
			sb.HarnessPin().Reference, sb.HarnessPin().Digest, claude.Bundle, claude.BundleDigest)
	}
}

func TestImportMarksItsSourcesDelivered(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	local := "/home/dev/src"
	archive := exportArchive(t, func(m *sandboxexport.Manifest) {
		m.Sandbox.Manifest.Source = &model.GitSource{
			Kind: "git", Delivery: model.GitSourceDeliveryPush,
			LocalDirectory: &local,
		}
	}, map[string]string{"sources/primary/main.go": "package main\n"})

	result, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(archive), services.SandboxImportOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	sb := result.Sandbox
	// The workspace arrived with the tree. Without this the sandbox parks at
	// awaiting_source for a push nobody is going to make (ADR 0123 §4).
	if sb.SourceDeliveredAt == nil {
		t.Fatal("sourceDeliveredAt is unset; the imported discobox would wait forever for a push")
	}
	if sb.Source.Delivery != model.GitSourceDeliveryPush {
		t.Errorf("delivery = %q; it is written into the bytes that were restored and must not be re-decided", sb.Source.Delivery)
	}
	if sb.SourceRoot == nil || *sb.SourceRoot != local {
		t.Errorf("sourceRoot = %v, want %q", sb.SourceRoot, local)
	}
}

func TestImportResolvesTheHarnessByNameAndRefusesAMissingOne(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	archive := exportArchive(t, func(m *sandboxexport.Manifest) {
		m.Sandbox.Harness = sandboxexport.Harness{Slug: "claude", Name: "Claude Code"}
	}, nil)

	_, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(archive), services.SandboxImportOptions{})
	if err == nil {
		t.Fatal("an import naming a harness this project does not have was accepted")
	}
	// The refusal says which harness, in the words the user knows it by.
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("err = %q; it should name the harness that is missing", err)
	}

	// --harness names another, which is the way through, by its slug, its
	// name or its ID.
	config, err := st.GetHarnessConfigBySlug(ctx, "project-1", "codex")
	if err != nil {
		t.Fatal(err)
	}
	for i, harness := range []string{"codex", "Codex", config.ID} {
		archive := exportArchive(t, func(m *sandboxexport.Manifest) {
			m.Sandbox.Name = fmt.Sprintf("imported-%d", i)
			m.Sandbox.Harness = sandboxexport.Harness{Slug: "claude", Name: "Claude Code"}
		}, nil)
		result, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(archive), services.SandboxImportOptions{Harness: harness})
		if err != nil {
			t.Fatalf("import with --harness %s: %v", harness, err)
		}
		if result.Sandbox.HarnessConfigID == nil || *result.Sandbox.HarnessConfigID != config.ID {
			t.Errorf("--harness %s: harness config = %v, want %q", harness, result.Sandbox.HarnessConfigID, config.ID)
		}
	}
}

func TestImportBindsSecretsByNameAndWarnsAboutTheRest(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	bearerSecret(t, st, "github", "")
	archive := exportArchive(t, func(m *sandboxexport.Manifest) {
		m.Sandbox.Secrets = []sandboxexport.SecretBinding{
			{Env: "GITHUB_TOKEN", Secret: "github"},
			{Env: "OPENAI_API_KEY", Secret: "openai"},
		}
	}, nil)

	result, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(archive), services.SandboxImportOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	assignments, err := st.ListSandboxSecrets(ctx, "project-1", result.Sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 1 || assignments[0].EnvName != "GITHUB_TOKEN" {
		t.Fatalf("assignments = %#v, want only the binding whose secret exists here", assignments)
	}
	if assignments[0].Sentinel == "" {
		t.Error("the binding got no sentinel")
	}
	// The one that could not be matched is reported rather than silently
	// dropped, and does not fail the import.
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "openai") {
		t.Fatalf("warnings = %v, want one naming the missing secret", result.Warnings)
	}
}

// A destination harness that declares a required secret nobody here has bound
// is an ordinary condition, not a corrupt archive. Answering it after the tree
// has landed makes the user pay a multi-gigabyte upload for a 400 and leaves an
// orphan tree on the pool — and in a transfer the source is already stopped, so
// the retry is the whole upload again.
func TestImportRefusesBeforeTheTreeIsUploaded(t *testing.T) {
	ctx, svc, st, provider := transferFixture(t)
	config := configuredHarness(t, st, "codex", "Codex")
	config.Secrets = []model.HarnessConfigSecret{{Name: "OPENAI_API_KEY", Required: true}}
	if err := st.UpdateHarnessConfig(ctx, config); err != nil {
		t.Fatal(err)
	}
	archive := exportArchive(t, nil, map[string]string{"data/big": "pretend this is a workspace"})

	_, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(archive), services.SandboxImportOptions{})
	if err == nil {
		t.Fatal("an import was accepted although the destination harness has an unbound required secret")
	}
	if provider.imported != nil {
		t.Error("the tree was uploaded before the refusal")
	}
}

// An archive that lost its tail reads, to a plain tar reader, as a smaller
// workspace. The import refuses it as the uploader's mistake, and creates no
// discobox around what did arrive.
func TestImportRefusesAnArchiveWithoutItsChecksums(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	archive := exportArchive(t, nil, map[string]string{"data/a": "alpha", "data/b": "beta"})

	// Every member but the last, rewritten as a well-formed tar: exactly what a
	// stream cut at a member boundary looks like.
	var stripped bytes.Buffer
	writer := tar.NewWriter(&stripped)
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == sandboxexport.SumsName {
			continue
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(stripped.Bytes()), services.SandboxImportOptions{})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("err = %v, want a 400: the archive is what is wrong, not the pool", err)
	}
	if !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Errorf("err = %q; it should say what was missing", err)
	}
	sandboxes, err := st.ListSandboxes(ctx, "project-1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sandboxes) != 0 {
		t.Errorf("%d discoboxes were created from an incomplete archive", len(sandboxes))
	}
}

func TestImportRefusesANameThisProjectAlreadyUses(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	if err := st.CreateSandbox(ctx, &model.Sandbox{
		ID: "sb-existing", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "my-box",
	}); err != nil {
		t.Fatal(err)
	}
	archive := exportArchive(t, nil, nil)

	if _, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(archive), services.SandboxImportOptions{}); err == nil {
		t.Fatal("a duplicate name was accepted; a name is an addressable handle, not a label")
	}
	// --name is the way through.
	result, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(archive), services.SandboxImportOptions{Name: "my-box-2"})
	if err != nil {
		t.Fatalf("import with --name: %v", err)
	}
	if result.Sandbox.Name != "my-box-2" {
		t.Errorf("name = %q, want my-box-2", result.Sandbox.Name)
	}
}

func TestExportCarriesNoSecretValuesAndNamesItsHarness(t *testing.T) {
	ctx, svc, st, provider := transferFixture(t)
	config := configuredHarness(t, st, "codex", "Codex")
	secret := bearerSecret(t, st, "github", "")
	anonymous := &model.Secret{
		ProjectID: "project-1", Name: "inline", Type: model.SecretTypeToken, Anonymous: true,
		EncryptedValue: []byte(`{"token":"sk-inline"}`),
	}
	if err := st.CreateSecret(ctx, anonymous); err != nil {
		t.Fatal(err)
	}
	sb := &model.Sandbox{
		ID: "sb-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "my-box",
		SandboxManifest: model.SandboxManifest{
			HarnessConfigID: &config.ID, Bundle: config.Bundle, BundleDigest: codexBundleV2, HarnessMode: "run",
		},
	}
	if err := st.CreateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	deliver(t, st, "sb-1", secret.ID, "GITHUB_TOKEN", "sentinel-a")
	deliver(t, st, "sb-1", anonymous.ID, "INLINE", "sentinel-b")
	deliver(t, st, "sb-1", secret.ID, "ASKED", "sentinel-c")
	provider.exportTree = emptyTar(t)

	stream, err := svc.ExportSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	defer stream.Close()
	archive, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	manifest, tree, err := sandboxexport.Read(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()

	if manifest.Sandbox.Harness.Slug != "codex" {
		t.Errorf("harness slug = %q; the slug is the only handle that means the same thing on two servers", manifest.Sandbox.Harness.Slug)
	}
	if manifest.Sandbox.Manifest.HarnessConfigID != nil {
		t.Error("the harness config ID traveled; it names a row on this server only")
	}
	bindings := map[string]string{}
	for _, binding := range manifest.Sandbox.Secrets {
		bindings[binding.Env] = binding.Secret
	}
	if bindings["GITHUB_TOKEN"] != "github" || bindings["ASKED"] != "github" {
		t.Errorf("bindings = %v, want GITHUB_TOKEN and ASKED bound to github by name", bindings)
	}
	// An anonymous secret has no name to resolve on the other side.
	if _, ok := bindings["INLINE"]; ok {
		t.Error("binding INLINE traveled")
	}
	// Belt and braces: no sentinel, and nothing that looks like a token, is
	// anywhere in the file.
	for _, forbidden := range []string{"sentinel-a", "sentinel-b", "sentinel-c", "sk-abc", "sk-inline"} {
		if bytes.Contains(archive, []byte(forbidden)) {
			t.Errorf("the archive contains %q", forbidden)
		}
	}
}

func TestExportManifestIsValidJSON(t *testing.T) {
	ctx, svc, st, provider := transferFixture(t)
	config := configuredHarness(t, st, "codex", "Codex")
	if err := st.CreateSandbox(ctx, &model.Sandbox{
		ID: "sb-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "my-box",
		SandboxManifest: model.SandboxManifest{
			HarnessConfigID: &config.ID, Bundle: config.Bundle, BundleDigest: codexBundleV2,
			PlatformBundle: platformRef, PlatformBundleDigest: platformOld,
			AgentBundle: agentRef, AgentBundleDigest: agentV1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	provider.exportTree = emptyTar(t)

	stream, err := svc.ExportSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	archive, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(archive))
	header, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != sandboxexport.ManifestName {
		t.Fatalf("first entry = %q", header.Name)
	}
	var decoded map[string]any
	if err := json.NewDecoder(reader).Decode(&decoded); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if decoded["formatVersion"] == nil {
		t.Error("the manifest carries no format version")
	}
	// The pool comes off the row. A discobox whose provisioning failed has no
	// runtime state naming one, and is the discobox an export is most wanted
	// for -- broken here, worth taking somewhere that works.
	if provider.exportedPool != "pool-1" {
		t.Errorf("export addressed pool %q, want the one on the row", provider.exportedPool)
	}
	// The tree is read with the sandbox's own pinned bundles, the ones its
	// box applies, not whatever its harness config names now (ADR 0129 §1).
	want := []sandbox.BundlePin{
		{Name: "discobox-platform", Digest: platformOld},
		{Name: "discobox-harness-codex", Digest: codexBundleV2},
	}
	if !slices.Equal(provider.exported.Bundles, want) || provider.exported.Agent != (sandbox.BundlePin{Name: "discobox-agent", Digest: agentV1}) {
		t.Errorf("export read with %+v, want the sandbox's pins %+v and its agent", provider.exported, want)
	}
}

func emptyTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := tarsums.NewWriter(&buf).Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarNames(t *testing.T, archive []byte) []string {
	t.Helper()
	var names []string
	// Through tarsums, so the tree handed to the pool has to carry a SHA256SUMS
	// that matches it, and the sums themselves are not listed as a file.
	reader := tarsums.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
	return names
}

// The meta file travels in the tree, and the control plane's copy of it
// travels in the spec, so an imported discobox lists and filters by its
// description and tags before it has reported them from its new home. The copy
// arrives unobserved: the source's observation time is another host's clock,
// and the discobox's first report here must replace it whatever that clock
// says (ADR 0136).
func TestExportAndImportCarryTheMetaCopy(t *testing.T) {
	ctx, svc, st, provider := transferFixture(t)
	config := configuredHarness(t, st, "codex", "Codex")
	if err := st.CreateSandbox(ctx, &model.Sandbox{
		ID: "sb-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "my-box",
		SandboxManifest: model.SandboxManifest{HarnessConfigID: &config.ID, Bundle: config.Bundle, HarnessMode: "run"},
	}); err != nil {
		t.Fatal(err)
	}
	reported := sandboxmeta.Meta{Name: "the reaper", Description: "fix the reaper", Tags: map[string]string{"wip": "", "ticket": "ENG-12"}}
	if err := st.UpdateSandboxMeta(ctx, "project-1", "sb-1", reported, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	provider.exportTree = emptyTar(t)

	stream, err := svc.ExportSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	archive, err := io.ReadAll(stream)
	_ = stream.Close()
	if err != nil {
		t.Fatal(err)
	}
	manifest, tree, err := sandboxexport.Read(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	_ = tree.Close()
	if manifest.Sandbox.Description == nil || *manifest.Sandbox.Description != reported.Description {
		t.Fatalf("exported description = %v, want %q", manifest.Sandbox.Description, reported.Description)
	}
	if !reflect.DeepEqual(manifest.Sandbox.Tags, reported.Tags) {
		t.Fatalf("exported tags = %v, want %v", manifest.Sandbox.Tags, reported.Tags)
	}
	// The meta's name travels beside the configured one, not in place of it.
	if manifest.Sandbox.MetaName == nil || *manifest.Sandbox.MetaName != reported.Name || manifest.Sandbox.Name != "my-box" {
		t.Fatalf("exported meta name = %v and name = %q, want %q and my-box", manifest.Sandbox.MetaName, manifest.Sandbox.Name, reported.Name)
	}

	result, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(archive), services.SandboxImportOptions{Name: "moved"})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	got, err := st.GetSandbox(ctx, "project-1", result.Sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description == nil || *got.Description != reported.Description || !reflect.DeepEqual(got.Tags, reported.Tags) {
		t.Fatalf("imported meta = %v %v, want %v", got.Description, got.Tags, reported)
	}
	if got.MetaName == nil || *got.MetaName != reported.Name || got.Name != "moved" {
		t.Fatalf("imported meta name = %v and name = %q, want %q and moved", got.MetaName, got.Name, reported.Name)
	}
	if got.MetaObservedAt != nil {
		t.Fatalf("imported metaObservedAt = %v, want unobserved", got.MetaObservedAt)
	}
	// Its first report here lands, even stamped an hour before the source's.
	if err := st.UpdateSandboxMeta(ctx, "project-1", got.ID, sandboxmeta.Meta{Tags: map[string]string{"moved": ""}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _ = st.GetSandbox(ctx, "project-1", got.ID); !reflect.DeepEqual(got.Tags, map[string]string{"moved": ""}) {
		t.Fatalf("tags after the first report = %v, want the report's", got.Tags)
	}
}

// An archive is not trusted to hold a valid tag set; one that does not is
// dropped rather than failing the import, and the discobox's first report
// fills the copy in.
func TestImportDropsInvalidTags(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	archive := exportArchive(t, func(m *sandboxexport.Manifest) {
		m.Sandbox.Tags = map[string]string{"bad key": ""}
	}, nil)
	result, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(archive), services.SandboxImportOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(result.Sandbox.Tags) != 0 {
		t.Fatalf("imported tags = %v, want none", result.Sandbox.Tags)
	}
}

// A discobox moves between machines, not between platforms (ADR 0145 §8):
// an archive of another platform than the pool it is imported onto is
// refused, before the tree is uploaded, and one of the same platform is
// recorded as that platform. Its harness rules no platform out.
func TestImportRefusesAnotherPlatform(t *testing.T) {
	ctx, svc, st, provider := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	// The pool's agent says it hosts another platform than the archive's.
	riscv := platform.Platform{OS: "linux", Arch: "riscv64"}
	if _, err := st.UpdatePoolStatus(ctx, "pool-1", riscv, platform.OCI, true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	archive := exportArchive(t, func(m *sandboxexport.Manifest) { m.Sandbox.Platform = platform.Pool() }, nil)
	_, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(archive), services.SandboxImportOptions{})
	if err == nil || !strings.Contains(err.Error(), riscv.String()) {
		t.Fatalf("err = %v, want a refusal naming the pool's platform", err)
	}
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusConflict {
		t.Errorf("err = %v, want a 409", err)
	}
	if provider.imported != nil {
		t.Error("the tree was uploaded before the platform was refused")
	}

	if _, err := st.UpdatePoolStatus(ctx, "pool-1", platform.Pool(), platform.OCI, true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	result, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(archive), services.SandboxImportOptions{})
	if err != nil {
		t.Fatalf("import onto its own platform: %v", err)
	}
	if result.Sandbox.Platform != platform.Pool() {
		t.Errorf("platform = %q, want %q", result.Sandbox.Platform, platform.Pool())
	}
}

// The export records the platform the discobox runs on.
func TestExportRecordsThePlatform(t *testing.T) {
	ctx, svc, st, provider := transferFixture(t)
	config := configuredHarness(t, st, "codex", "Codex")
	sb := &model.Sandbox{
		ID: "sb-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "my-box",
		Platform: platform.Pool(),
		SandboxManifest: model.SandboxManifest{
			HarnessConfigID: &config.ID, Bundle: config.Bundle, BundleDigest: codexBundleV2, HarnessMode: "run",
		},
	}
	if err := st.CreateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	provider.exportTree = emptyTar(t)
	stream, err := svc.ExportSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	defer stream.Close()
	manifest, tree, err := sandboxexport.Read(stream)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if manifest.Sandbox.Platform != platform.Pool() {
		t.Errorf("platform = %q, want %q", manifest.Sandbox.Platform, platform.Pool())
	}
}

// A sandbox not placed since platforms were recorded has no platform of its
// own, and is exported with its pool's, which is where its tree runs.
func TestExportTakesThePoolsPlatformForASandboxWithNone(t *testing.T) {
	ctx, svc, st, provider := transferFixture(t)
	config := configuredHarness(t, st, "codex", "Codex")
	riscv := platform.Platform{OS: "linux", Arch: "riscv64"}
	if _, err := st.UpdatePoolStatus(ctx, "pool-1", riscv, platform.OCI, true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	sb := &model.Sandbox{
		ID: "sb-1", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", Name: "my-box",
		SandboxManifest: model.SandboxManifest{HarnessConfigID: &config.ID, Bundle: config.Bundle, BundleDigest: codexBundleV2, HarnessMode: "run"},
	}
	if err := st.CreateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	provider.exportTree = emptyTar(t)
	stream, err := svc.ExportSandbox(ctx, "project-1", "sb-1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	defer stream.Close()
	manifest, tree, err := sandboxexport.Read(stream)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if manifest.Sandbox.Platform != riscv {
		t.Errorf("platform = %q, want the pool's %q", manifest.Sandbox.Platform, riscv)
	}
}

// An archive from before platforms names none and is not guessed at: it is
// imported onto a pool of any platform — here one this machine's architecture
// is not — and the sandbox it becomes has no platform until it is placed.
func TestImportOfAnArchiveFromBeforePlatformsGuessesNone(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	riscv := platform.Platform{OS: "linux", Arch: "riscv64"}
	if _, err := st.UpdatePoolStatus(ctx, "pool-1", riscv, platform.OCI, true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	archive := exportArchive(t, nil, map[string]string{"data/x": "x"})
	result, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(archive), services.SandboxImportOptions{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !result.Sandbox.Platform.IsZero() {
		t.Fatalf("platform = %q, want none guessed", result.Sandbox.Platform)
	}
}

// A pool whose agent has not reported yet — a new machine's, where a transfer
// usually goes — is not refused up front: the import goes on to the provider,
// whose placement waits for the pool and checks the archive's platform once
// the pool declares one (store.SchedulablePoolForSandbox, which refuses a pool
// that never does).
func TestImportWaitsOnAPoolThatHasNotDeclaredYet(t *testing.T) {
	ctx, svc, st, provider := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	archive := exportArchive(t, func(m *sandboxexport.Manifest) { m.Sandbox.Platform = platform.Pool() }, map[string]string{"data/x": "x"})
	result, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(archive), services.SandboxImportOptions{})
	if err != nil {
		t.Fatalf("import onto a pool that has not reported yet: %v", err)
	}
	if provider.importedPool != "pool-1" || result.Sandbox.Platform != platform.Pool() {
		t.Fatalf("tree landed on %q with platform %q, want pool-1 and the archive's %q", provider.importedPool, result.Sandbox.Platform, platform.Pool())
	}
}

// An archive is a container discobox's tree, and a machine discobox is not
// imported (ADR 26-10-09-106 defers it): it lands only on a pool that runs OCI
// images, and the pool's kind, not its harness's, is the refusal, made before
// its tree is read (ADR 26-11-01-122).
func TestImportRefusesAPoolOfAnotherImageKind(t *testing.T) {
	ctx, svc, st, provider := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	if _, err := st.UpdatePoolStatus(ctx, "pool-1", platform.Pool(), platform.VM("boxd"), true, true, false, 1, 1, 1, nil); err != nil {
		t.Fatal(err)
	}
	archive := exportArchive(t, func(m *sandboxexport.Manifest) { m.Sandbox.Platform = platform.Pool() }, map[string]string{"data/x": "x"})
	_, err := svc.ImportSandbox(ctx, "project-1", bytes.NewReader(archive), services.SandboxImportOptions{})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusConflict || !strings.Contains(err.Error(), "the pool runs VM images for the boxd driver") {
		t.Fatalf("err = %v, want a 409 naming the pool's kind", err)
	}
	if provider.imported != nil {
		t.Error("the tree was uploaded before the kind was refused")
	}
}
