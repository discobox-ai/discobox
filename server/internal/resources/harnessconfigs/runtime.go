package harnessconfigs

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// HarnessConfigResourceType drives durable capture acknowledgments.
const HarnessConfigResourceType = "harnessConfig"

// SandboxRuntime owns the sandbox operations used by save-back, capture
// delivery and config-change fan-outs.
//
// AcquireSandboxHTTPClient authorizes the caller's scopes, so a save a person
// makes happens inside their request with their credentials; delivery of a
// committed capture receipt uses the explicit server-only lease.
type SandboxRuntime interface {
	AcquireSandboxHTTPClientForServer(context.Context, string, string, []string, services.SandboxUse) (*services.HTTPClientLease, *model.Sandbox, error)
	AcquireSandboxHTTPClient(ctx context.Context, projectID, sandboxID string, scopes []string, use services.SandboxUse) (*services.HTTPClientLease, *model.Sandbox, error)
	// MarkBindingHoldersDirtyTx marks the sandboxes the bindings' targets
	// cover, in the transaction that wrote or removed them, so each is
	// materialized again and its pool sent the result (ADR 26-10-27-880 §2).
	MarkBindingHoldersDirtyTx(ctx context.Context, txStore *store.Store, txDB *gorm.DB, projectID string, bindings []model.SecretBinding) error
	// MaterializeSandboxSecretsTx makes one sandbox's sentinels its bindings
	// now, in the caller's transaction: a save-back hands the sandbox that
	// saved its new credentials' sentinels in its acknowledgment.
	MaterializeSandboxSecretsTx(ctx context.Context, txStore *store.Store, projectID, sandboxID string) error
	// MarkSecretHoldersDirtyTx marks the sandboxes holding a secret for
	// reconcile in the transaction that changes its routes, so each converges
	// on them (ADR 26-10-02-924).
	MarkSecretHoldersDirtyTx(ctx context.Context, txStore *store.Store, txDB *gorm.DB, projectID, secretID string) error
	// UpgradeHarnessConfigSandboxes moves this config's stopped sandboxes onto
	// the bundle it now resolves to (ADR 0082). Called wherever this config's
	// BundleDigest or ManifestDigest is written to a different value,
	// through applyResolvedPins.
	UpgradeHarnessConfigSandboxes(ctx context.Context, projectID, harnessConfigID string) error
	// MarkProxyPolicyChangedTx converges this config's discoboxes onto a changed
	// egress level — a person's, or the hosts its image declares
	// (ADR 26-10-01-032 §5) — in the transaction that changes it.
	MarkProxyPolicyChangedTx(ctx context.Context, txStore *store.Store, txDB *gorm.DB, projectID, poolID, harnessConfigID string) error
}

// Dirtier schedules reconciliation.
type Dirtier interface {
	MarkDirtyTx(context.Context, *gorm.DB, string, string) error
	MarkDirtyAt(ctx context.Context, resourceType, id string, at time.Time) error
}

// SetSandboxRuntime installs this package's seam onto sandboxes: save-back's
// agent access, and the fan-outs that follow a config change — its secret
// bindings, and its image (ADR 0082).
func (s *Service) SetSandboxRuntime(runtime SandboxRuntime) { s.sandboxes = runtime }

// SetDirtier installs durable capture delivery scheduling.
func (s *Service) SetDirtier(dirtier Dirtier) { s.dirtier = dirtier }

// Reconcile delivers committed capture acknowledgments. It uses an explicit
// server-only lease and never rereads or resubmits captured credentials.
func (s *Service) Reconcile(ctx context.Context, configID string) (reconcile.Result, error) {
	var config *model.HarnessConfig
	err := s.store.Transaction(ctx, func(tx *store.Store, _ *gorm.DB) error {
		var err error
		config, err = tx.GetHarnessConfigByID(ctx, configID)
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, s.reconcileCaptureAcknowledgments(ctx, config)
}
