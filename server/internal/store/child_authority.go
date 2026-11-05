package store

import (
	"context"
	"fmt"
	"slices"

	"github.com/discobox-ai/discobox/server/internal/model"
)

// ChildAuthorityVersion reads committed notifications from the primary, never
// a potentially lagging read replica. The key set follows both live sandboxes.
func (s *Store) ChildAuthorityVersion(ctx context.Context, project, caller, target string, write bool) (string, error) {
	db, err := s.getWrite(ctx)
	if err != nil {
		return "", err
	}
	var sandboxes []model.Sandbox
	if err := db.Where("project_id = ? AND id IN ?", project, []string{caller, target}).Find(&sandboxes).Error; err != nil {
		return "", err
	}
	keys := []string{"sandboxes/" + caller, "sandboxes/" + target, "projects/" + project}
	for _, sb := range sandboxes {
		keys = append(keys, "pools/"+sb.PoolID)
		if sb.HarnessConfigID != nil {
			keys = append(keys, "harness_configs/"+*sb.HarnessConfigID)
		}
	}
	if write {
		for _, key := range slices.Clone(keys) {
			keys = append(keys, "egress/"+key)
		}
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	var rows []struct {
		Resource string
		Version  int64
	}
	if err := db.Table("child_authority_versions").Where("resource IN ?", keys).Order("resource").Find(&rows).Error; err != nil {
		return "", err
	}
	return fmt.Sprint(keys, rows), nil
}
