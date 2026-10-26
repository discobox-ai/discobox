package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/server/internal/model"
	"gorm.io/gorm"
)

// EffectiveSandboxEgress is a sandbox's effective egress policy
// (ADR 26-10-01-032 §2): the merge of its project's, pool's, harness config's
// (with the hosts its image declares), and its own level, as they stand now.
// It is never stored, so there is no copy of it to go stale. sb need not be a
// stored row: a create checks the policy a new sandbox would have.
//
// It is here rather than with the sandbox service because two owners read it:
// the sandbox service, which delivers it, and the sandbox role's authorizer,
// which bounds what a discobox may act on by it. A harness config that is gone
// contributes nothing.
func (s *Store) EffectiveSandboxEgress(ctx context.Context, sb *model.Sandbox) (egress.Policy, error) {
	levels, err := s.machineEgressLevels(ctx, sb)
	if err != nil {
		return egress.Policy{}, err
	}
	return egress.Merge(append(levels, sb.Egress)...), nil
}

// ProvisionEgress is the egress policy for provisioning a sandbox's
// machine-scoped layers on no sandbox's behalf (ADR 26-10-16-423 §§1, 8):
// EffectiveSandboxEgress without the sandbox's own level, so the project's,
// the pool's and the harness config's (with the hosts its image declares).
// It names nothing of the sandbox, so what is provisioned under it is the
// same for every sandbox of the plan.
func (s *Store) ProvisionEgress(ctx context.Context, sb *model.Sandbox) (egress.Policy, error) {
	levels, err := s.machineEgressLevels(ctx, sb)
	if err != nil {
		return egress.Policy{}, err
	}
	return egress.Merge(levels...), nil
}

// machineEgressLevels is every egress level of sb but its own: its
// project's, its pool's, and its harness config's with the hosts its image
// declares.
func (s *Store) machineEgressLevels(ctx context.Context, sb *model.Sandbox) ([]egress.Policy, error) {
	project, err := s.GetProject(ctx, sb.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("read project egress: %w", err)
	}
	pool, err := s.GetPool(ctx, sb.ProjectID, sb.PoolID)
	if err != nil {
		return nil, fmt.Errorf("read pool egress: %w", err)
	}
	levels := []egress.Policy{project.Egress, pool.Egress}
	if sb.HarnessConfigID != nil && strings.TrimSpace(*sb.HarnessConfigID) != "" {
		harnessConfig, err := s.GetHarnessConfig(ctx, sb.ProjectID, strings.TrimSpace(*sb.HarnessConfigID))
		switch {
		case err == nil:
			levels = append(levels, harnessConfig.Egress, egress.Policy{Allow: harnessConfig.ImageEgressHosts})
		case !errors.Is(err, ErrNotFound):
			return nil, fmt.Errorf("read harness config egress: %w", err)
		}
	}
	return levels, nil
}

// egressColumn is the egress level of a project, a harness config, or a
// sandbox (ADR 26-10-01-032), and is written by create and by its own setter
// (setEgress) alone: every whole-row save of those three omits it. A level
// change bumps no generation and takes no lock a whole-row writer holds, so a
// writer that loaded the row before the change — a reconcile, the configure
// flow committing, a default being set — would otherwise save the old level
// back over the new one, and the next push would deliver it: a person's
// tightening silently undone. A pool's whole-row update names its columns, and
// names this one only for the person's own update (UpdatePool).
const egressColumn = "egress"

// SetProjectEgress writes a project's egress level, and nothing else.
func (s *Store) SetProjectEgress(ctx context.Context, projectID string, level egress.Policy) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return updateEgress(write.WithContext(ctx).Model(&model.Project{}).Where("id = ?", projectID), level)
}

// SetPoolEgress writes a pool's egress level, and nothing else.
func (s *Store) SetPoolEgress(ctx context.Context, projectID, poolID string, level egress.Policy) error {
	return s.setEgress(ctx, &model.Pool{}, projectID, poolID, level)
}

// SetHarnessConfigEgress writes the egress level a person set on a harness
// config, and nothing else.
func (s *Store) SetHarnessConfigEgress(ctx context.Context, projectID, configID string, level egress.Policy) error {
	encoded, err := json.Marshal(level)
	if err != nil {
		return err
	}
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	result := write.Model(&model.HarnessConfig{}).Where("project_id = ? AND id = ?", projectID, configID).Updates(map[string]any{egressColumn: string(encoded), "revision": gorm.Expr("revision + 1")})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) setEgress(ctx context.Context, row any, projectID, id string, level egress.Policy) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return updateEgress(write.WithContext(ctx).Model(row).Where("project_id = ? AND id = ?", projectID, id), level)
}

func updateEgress(query *gorm.DB, level egress.Policy) error {
	// Encoded here rather than left to the field's serializer, which a
	// column update does not apply.
	encoded, err := json.Marshal(level)
	if err != nil {
		return err
	}
	result := query.Update(egressColumn, string(encoded))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
