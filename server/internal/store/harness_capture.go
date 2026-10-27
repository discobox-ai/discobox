package store

import (
	"context"
	"strconv"
	"time"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// CaptureBaseline snapshots the stored config and credential identities. Call
// through the transaction store whenever this snapshot decides a write.
func (s *Store) CaptureBaseline(ctx context.Context, projectID, configID string) (harnesscapture.Baseline, error) {
	cfg, err := s.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return harnesscapture.Baseline{}, err
	}
	b := harnesscapture.Baseline{HarnessConfigID: cfg.ID, ConfigRevision: cfg.Revision, ManifestDigest: cfg.ManifestDigest, SecretRevisions: map[string]string{}}
	indexes := map[string]int{}
	for _, files := range [][]model.HarnessConfigFile{cfg.Files, cfg.ConfiguredFiles} {
		for _, f := range files {
			file := harnesscapture.File{Path: f.Path, Content: f.Content, CreateOnly: f.CreateOnly, Template: f.Template}
			if i, ok := indexes[f.Path]; ok {
				b.Files[i] = file
			} else {
				indexes[f.Path] = len(b.Files)
				b.Files = append(b.Files, file)
			}
		}
	}
	bindings, err := s.ListHarnessConfigSecretBindings(ctx, projectID, cfg.ID)
	if err != nil {
		return b, err
	}
	for _, binding := range bindings {
		secret, err := s.GetSecret(ctx, projectID, binding.SecretID)
		if err != nil {
			return b, err
		}
		b.SecretRevisions[binding.EnvName] = secret.ID + ":" + strconv.FormatInt(secret.Revision, 10)
	}
	return b, nil
}

// GetHarnessCaptureReceipt reads the primary: a replay must see the commit
// immediately even when ordinary resource reads use a replica.
func (s *Store) GetHarnessCaptureReceipt(ctx context.Context, projectID, sandboxID, configID, candidateID string) (*model.HarnessCaptureReceipt, error) {
	db, err := s.getWrite(ctx)
	if err != nil {
		return nil, err
	}
	var r model.HarnessCaptureReceipt
	err = db.Where("project_id = ? AND sandbox_id = ? AND harness_config_id = ? AND candidate_id = ?", projectID, sandboxID, configID, candidateID).First(&r).Error
	return &r, mapNotFound(err)
}
func (s *Store) CreateHarnessCaptureReceipt(ctx context.Context, r *model.HarnessCaptureReceipt) error {
	db, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return db.Create(r).Error
}

// ListPendingHarnessCaptureReceipts reads the primary so replica lag cannot
// make a reconciler clear the only dirty mark for a newly accepted receipt.
func (s *Store) ListPendingHarnessCaptureReceipts(ctx context.Context, projectID, configID string) ([]model.HarnessCaptureReceipt, error) {
	db, err := s.getWrite(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.HarnessCaptureReceipt
	err = db.Where("project_id = ? AND harness_config_id = ? AND acknowledged_at IS NULL AND superseded = ?", projectID, configID, false).Order("created_at ASC").Find(&out).Error
	return out, err
}
func (s *Store) FinishHarnessCaptureReceipt(ctx context.Context, r *model.HarnessCaptureReceipt, superseded bool) error {
	db, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	columns := map[string]any{"superseded": superseded}
	if !superseded {
		columns["acknowledged_at"] = time.Now().UTC()
	}
	return db.Model(&model.HarnessCaptureReceipt{}).Where("project_id = ? AND sandbox_id = ? AND harness_config_id = ? AND candidate_id = ?", r.ProjectID, r.SandboxID, r.HarnessConfigID, r.CandidateID).Updates(columns).Error
}
