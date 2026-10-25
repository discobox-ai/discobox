package sandboxes

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/sourcecache"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

func TestSourceCompletionMergesPartialReportsAndRepeatsAfterStart(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	commit := strings.Repeat("a", 40)
	primary, extra := "primary", "extra"
	sb := &model.Sandbox{ID: "sbx-partial", ProjectID: "project-1", CreatedByUserID: "user-1", Name: "partial", PoolID: "pool-1", SandboxManifest: model.SandboxManifest{Source: &model.GitSource{Kind: "git", Delivery: "cache", Slug: &primary, Checkout: &model.GitSourceCheckout{Commit: &commit}}, SourceCodeReferences: model.SourceCodeReferences{"extra": {Kind: "git", Delivery: "cache", Slug: &extra, Checkout: &model.GitSourceCheckout{Commit: &commit}}}}}
	sb.State = model.SandboxStateAwaitingSource
	sb.DesiredState = model.DesiredStatePresent
	if err := st.CreateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	first, err := svc.CompleteSandboxSourcePush(ctx, "project-1", sb.ID, services.CompleteSandboxSourcePushBody{Sources: map[string]string{primary: commit}})
	if err != nil {
		t.Fatal(err)
	}
	if first.SourceDeliveredAt != nil || first.SourceDeliveries[primary].Status != sourcecache.Delivered {
		t.Fatal("partial report resumed sandbox or lost source")
	}
	second, err := svc.CompleteSandboxSourcePush(ctx, "project-1", sb.ID, services.CompleteSandboxSourcePushBody{Sources: map[string]string{extra: commit}})
	if err != nil {
		t.Fatal(err)
	}
	if second.SourceDeliveredAt == nil || second.SourceDeliveries[primary].Status != sourcecache.Delivered {
		t.Fatal("last report did not complete both sources")
	}
	second.State = model.SandboxStateReady
	if err := st.UpdateSandbox(ctx, second); err != nil {
		t.Fatal(err)
	}
	repeat, err := svc.CompleteSandboxSourcePush(ctx, "project-1", sb.ID, services.CompleteSandboxSourcePushBody{Sources: map[string]string{primary: commit}})
	if err != nil || repeat.Generation != second.Generation {
		t.Fatalf("idempotent completion changed intent: %v", err)
	}
	if _, err := svc.CompleteSandboxSourcePush(ctx, "project-1", sb.ID, services.CompleteSandboxSourcePushBody{Sources: map[string]string{primary: strings.Repeat("b", 40)}}); err == nil {
		t.Fatal("accepted a different commit after delivery")
	}
}
