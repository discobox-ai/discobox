package harnessconfigs

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

// deliverCaptureAcknowledgment sends only a durable receipt. It is deliberately
// separate from the user-authorized capture request: reconciliation has no user
// principal, and authority to deliver this candidate comes from its commit.
func (s *Service) deliverCaptureAcknowledgment(ctx context.Context, r *model.HarnessCaptureReceipt) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if r.Superseded {
		return "saved-handoff-superseded", nil
	}
	if r.AcknowledgedAt != nil {
		return "saved", nil
	}
	var box *model.Sandbox
	var current harnesscapture.Baseline
	var assignments []model.SandboxSecret
	// Safety decisions use a coherent primary snapshot, never a lagging replica.
	err := s.store.Transaction(ctx, func(tx *store.Store, _ *gorm.DB) error {
		var err error
		box, err = tx.GetSandbox(ctx, r.ProjectID, r.SandboxID)
		if err != nil {
			return err
		}
		current, err = tx.CaptureBaseline(ctx, r.ProjectID, r.HarnessConfigID)
		if err != nil {
			return err
		}
		assignments, err = tx.ListInjectedSandboxSecrets(ctx, r.ProjectID, r.SandboxID)
		return err
	})
	if err != nil {
		return "", err
	}
	superseded := box.HarnessConfigID == nil || *box.HarnessConfigID != r.HarnessConfigID || current.ManifestDigest != r.Baseline.ManifestDigest || !sameCaptureBindings(current.SecretRevisions, r.Baseline.SecretRevisions)
	ack := harnesscapture.Acknowledgment{CandidateID: r.CandidateID, Baseline: r.Baseline, Superseded: superseded, Sentinels: map[string]string{}}
	if !superseded {
		for _, assignment := range assignments {
			ack.Sentinels[assignment.EnvName] = assignment.Sentinel
		}
		// Conditional revisions advance, but the handoff contains only templates
		// accepted by this receipt, never unrelated later settings.
		ack.Baseline = current
		ack.Files = r.Baseline.Files
	}
	if s.sandboxes == nil {
		return "", errors.New("harness capture requires sandbox runtime")
	}
	lease, leasedBox, err := s.sandboxes.AcquireSandboxHTTPClientForServer(ctx, r.ProjectID, r.SandboxID, []string{harnesscapture.ScopeWrite}, services.SandboxRead)
	if err != nil {
		return "", err
	}
	defer lease.Release()
	var preview harnesscapture.Preview
	if err := s.sendCaptureRequest(ctx, r.ProjectID, r.SandboxID, http.MethodPost, "/harness-config-ack", ack, &preview, lease, leasedBox); err != nil {
		return "", err
	}
	if err := s.store.FinishHarnessCaptureReceipt(ctx, r, superseded); err != nil {
		return "", err
	}
	return preview.State, nil
}

// Upstream refresh changes the value revision but not what its sentinel names.
// Deletion/rebinding changes identity and must not acknowledge an old binding.
func sameCaptureBindings(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for env, revision := range a {
		other, ok := b[env]
		if !ok || strings.SplitN(revision, ":", 2)[0] != strings.SplitN(other, ":", 2)[0] {
			return false
		}
	}
	return true
}

func (s *Service) reconcileCaptureAcknowledgments(ctx context.Context, cfg *model.HarnessConfig) error {
	pending, err := s.store.ListPendingHarnessCaptureReceipts(ctx, cfg.ProjectID, cfg.ID)
	if err != nil {
		return err
	}
	var failures []error
	for i := range pending {
		if _, err := s.deliverCaptureAcknowledgment(ctx, &pending[i]); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
