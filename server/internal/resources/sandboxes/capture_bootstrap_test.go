package sandboxes

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
	"gorm.io/gorm"
)

func TestCreateOptionsCarryCaptureBaselineOfDeliveredFiles(t *testing.T) {
	_, st := newUpgradeEngineFixture(t)
	ctx := context.Background()
	sb := egressFixture(t, st, egress.Policy{}, egress.Policy{}, egress.Policy{}, egress.Policy{})
	cfg, err := st.GetHarnessConfig(ctx, sb.ProjectID, *sb.HarnessConfigID)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ManifestDigest = "sha256:manifest"
	cfg.SaveBack = &harnesscapture.Definition{Version: 1, Watch: []string{"~/.settings.json"}, ExportCommand: []string{"/bin/export"}, HandoffCommand: []string{"/bin/handoff"}}
	cfg.Files = []model.HarnessConfigFile{{Path: "~/.settings.json", Content: `{"model":"image"}`}, {Path: "~/.other.json", Content: `{}`}}
	cfg.ConfiguredFiles = []model.HarnessConfigFile{{Path: "~/.settings.json", Content: `{"model":"configured"}`}}
	if err := st.UpdateHarnessConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	opts, err := (&SandboxReconciler{store: st}).createOptionsFromSandbox(ctx, sb)
	if err != nil {
		t.Fatal(err)
	}
	got := opts.ResolvedHarnessConfig
	if !reflect.DeepEqual(got.SaveBack, cfg.SaveBack) || got.CaptureBaseline.ConfigRevision != cfg.Revision || got.CaptureBaseline.ManifestDigest != cfg.ManifestDigest {
		t.Fatal("config provenance did not reach create options")
	}
	if len(got.CaptureBaseline.Files) != 2 || got.CaptureBaseline.Files[0].Content != `{"model":"configured"}` {
		t.Fatalf("baseline did not preserve configured overlay: %+v", got.CaptureBaseline.Files)
	}
	if got.Files[0].Content != `{"model":"image"}` || got.ConfiguredFiles[0].Content != `{"model":"configured"}` {
		t.Fatal("baseline computation collapsed ownership layers")
	}
}

func TestCreateOptionsRefuseConfigRevisionChangedBetweenSnapshots(t *testing.T) {
	_, st := newUpgradeEngineFixture(t)
	ctx := context.Background()
	sb := egressFixture(t, st, egress.Policy{}, egress.Policy{}, egress.Policy{}, egress.Policy{})
	// The first two config queries resolve runtime/provision egress. Edit just
	// after the third (the delivered config snapshot), before CaptureBaseline
	// loads its config. This deterministically exercises the split-read race.
	queries := 0
	if err := st.Transaction(ctx, func(_ *store.Store, db *gorm.DB) error {
		return db.Callback().Query().After("gorm:query").Register("test:capture_snapshot_edit", func(q *gorm.DB) {
			if q.Statement.Table != "harness_configs" {
				return
			}
			queries++
			if queries != 3 {
				return
			}
			if err := q.Session(&gorm.Session{NewDB: true}).Model(&model.HarnessConfig{}).Where("id = ?", *sb.HarnessConfigID).UpdateColumn("revision", gorm.Expr("revision + 1")).Error; err != nil {
				_ = q.AddError(err)
			}
		})
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = st.Transaction(ctx, func(_ *store.Store, db *gorm.DB) error {
			return db.Callback().Query().Remove("test:capture_snapshot_edit")
		})
	})
	if _, err := (&SandboxReconciler{store: st}).createOptionsFromSandbox(ctx, sb); !errors.Is(err, store.ErrGenerationConflict) {
		t.Fatalf("mixed revision delivery accepted: %v (%d reads)", err, queries)
	}
}
