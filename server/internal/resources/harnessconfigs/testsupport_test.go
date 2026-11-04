package harnessconfigs

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/harnessdefs"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// stubInspector answers what byImage holds, by reference: the manifest the
// entry holds as that bundle's own label, digested as the entry is.
// The platform bundle is stubPlatformLayer unless byImage names it.
type stubInspector struct {
	byImage map[string]imageMetadata
	// platformEnv is env the platform bundle's layer declares.
	platformEnv map[string]string
}

func (s *stubInspector) InspectBundle(_ context.Context, bundle string, admit func(map[string]string) error) (bundleInspection, error) {
	inspected, ok := s.byImage[bundle]
	read := stubBundle(inspected.BundleDigest, inspected.ImageMetadata)
	if !ok && bundle == testPlatformBundle {
		read = stubPlatformBundle(s.platformEnv)
	}
	if err := admit(read.Labels); err != nil {
		return bundleInspection{}, err
	}
	return read, nil
}

// admitAny admits every bundle, for a test of what an inspector reads rather
// than of what is admitted.
func admitAny(map[string]string) error { return nil }

// stubBundle is a harness bundle declaring metadata as its own label, on the
// platform's API version.
func stubBundle(digest string, metadata harness.ImageMetadata) bundleInspection {
	metadata.ImageKind, metadata.PlatformAPIVersion = platform.ImageKind{}, harness.PlatformAPIVersion
	label, err := json.Marshal(metadata)
	if err != nil {
		panic(err)
	}
	return bundleInspection{Digest: digest, Labels: map[string]string{harness.ImageLabel: string(label)}}
}

// testPlatformBundle is the platform bundle a test service composes on, which
// stubInspector answers with stubPlatformBundle.
const testPlatformBundle = "discobox-platform:test"

// testReferences names the platform bundle and no built-in override.
var testReferences = harnessdefs.References{PlatformBundle: testPlatformBundle}

// bundleReferences names each built-in's bundle, which stubInspector answers
// for, on testPlatformBundle.
func bundleReferences(refs map[string]string) harnessdefs.References {
	return harnessdefs.References{PlatformBundle: testPlatformBundle, Bundles: refs}
}

// stubPlatformBundle is a platform bundle whose layer provides the platform
// API version and nothing else.
func stubPlatformBundle(env map[string]string) bundleInspection {
	layer, err := json.Marshal(map[string]any{"apiVersion": "discobox.dev/image/v1", "platformAPIVersion": harness.PlatformAPIVersion, "env": env})
	if err != nil {
		panic(err)
	}
	return bundleInspection{Digest: "sha256:platform", Labels: map[string]string{
		harness.ImageLayerLabelPrefix + harness.PlatformLayer: string(layer),
	}}
}

func newTestStore(t *testing.T, opts ...store.Option) *store.Store {
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
	if err := db.Write.WithContext(ctx).Create(&model.Project{
		ID: "project-1", OwnerUserID: "user-1", Name: "Project",
	}).Error; err != nil {
		t.Fatalf("create project: %v", err)
	}
	return store.New(db.Write, db.Read, opts...)
}

// stubSandboxRuntime records the fan-outs a config change asks for.
type stubSandboxRuntime struct {
	rebound       []string
	upgraded      []string
	marked        []string
	egressChanged []string
}

func (s *stubSandboxRuntime) MarkProxyPolicyChangedTx(_ context.Context, _ *store.Store, _ *gorm.DB, _, _, harnessConfigID string) error {
	s.egressChanged = append(s.egressChanged, harnessConfigID)
	return nil
}

func (s *stubSandboxRuntime) MarkSecretHoldersDirtyTx(_ context.Context, _ *store.Store, _ *gorm.DB, _, secretID string) error {
	s.marked = append(s.marked, secretID)
	return nil
}

func (s *stubSandboxRuntime) AcquireSandboxHTTPClientForServer(ctx context.Context, p, b string, scopes []string, use services.SandboxUse) (*services.HTTPClientLease, *model.Sandbox, error) {
	return s.AcquireSandboxHTTPClient(ctx, p, b, scopes, use)
}

// MarkBindingHoldersDirtyTx records the targets whose holders a binding
// change reached.
func (s *stubSandboxRuntime) MarkBindingHoldersDirtyTx(_ context.Context, _ *store.Store, _ *gorm.DB, _ string, bindings []model.SecretBinding) error {
	for _, binding := range bindings {
		if !slices.Contains(s.rebound, binding.TargetID) {
			s.rebound = append(s.rebound, binding.TargetID)
		}
	}
	return nil
}

func (s *stubSandboxRuntime) MaterializeSandboxSecretsTx(context.Context, *store.Store, string, string) error {
	return nil
}

// harnessBinding is a harness config's use of a secret in a variable, as the
// tests write it.
type harnessBinding struct {
	ProjectID, HarnessConfigID, EnvName, SecretID string
}

// putHarnessBinding binds a secret for use in a variable on a harness config,
// as configure does.
func putHarnessBinding(ctx context.Context, st *store.Store, b harnessBinding) error {
	_, err := st.PutTargetUseBinding(ctx, &model.SecretBinding{
		ProjectID: b.ProjectID, SecretID: b.SecretID, TargetKind: model.SecretBindingTargetHarnessConfig,
		TargetID: b.HarnessConfigID, Use: true, EnvName: b.EnvName,
	}, store.BindingChange{Actor: "user-1"})
	return err
}

func (s *stubSandboxRuntime) UpgradeHarnessConfigSandboxes(_ context.Context, _, harnessConfigID string) error {
	s.upgraded = append(s.upgraded, harnessConfigID)
	return nil
}

func (s *stubSandboxRuntime) AcquireSandboxHTTPClient(context.Context, string, string, []string, services.SandboxUse) (*services.HTTPClientLease, *model.Sandbox, error) {
	return nil, nil, errors.New("not used")
}

type stubDirtier struct{}

func (stubDirtier) MarkDirtyTx(context.Context, *gorm.DB, string, string) error { return nil }

func (stubDirtier) MarkDirtyAt(context.Context, string, string, time.Time) error { return nil }
