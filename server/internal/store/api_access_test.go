package store_test

import (
	"context"
	"sync"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
)

func TestAPIConsentIsAtomicSingleDecisionAndRetained(t *testing.T) {
	st := adoptionBoxes(t)
	ctx := context.Background()
	if err := st.SetSandboxDiscoboxAPI(ctx, "project-1", "a", false); err != nil {
		t.Fatal(err)
	}
	req := &model.APIAccessRequest{ProjectID: "project-1", RequesterSandboxID: "a", Justification: "Coordinate workers"}
	if err := st.CreateAPIAccessRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	duplicate := &model.APIAccessRequest{ProjectID: req.ProjectID, RequesterSandboxID: "a", Justification: "Different reason"}
	if err := st.CreateAPIAccessRequest(ctx, duplicate); err != nil || duplicate.ID != req.ID || duplicate.Justification != req.Justification {
		t.Fatalf("pending request changed: %#v %v", duplicate, err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, approve := range []bool{true, false} {
		wg.Go(func() {
			_, err := st.DecideAPIAccessRequest(ctx, req.ProjectID, req.ID, "person", approve)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("decisions succeeded=%d", successes)
	}
	decided, err := st.GetAPIAccessRequest(ctx, req.ProjectID, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	box, err := st.GetSandbox(ctx, req.ProjectID, "a")
	if err != nil {
		t.Fatal(err)
	}
	if box.DiscoboxAPI != (decided.Status == model.SecretRequestStatusApproved) || decided.DecidedBy != "person" || decided.DecidedAt == nil {
		t.Fatalf("setting and consent disagree: %#v %#v", box, decided)
	}
	if _, err := st.GetAPIAccessRequest(ctx, "another-project", req.ID); err == nil {
		t.Fatal("cross-project read succeeded")
	}
	if err := st.DeleteSandbox(ctx, req.ProjectID, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAPIAccessRequest(ctx, req.ProjectID, req.ID); err != nil {
		t.Fatalf("purge lost consent: %v", err)
	}
}

func TestAPIAccessApprovalRechecksSandboxAndDenialWorksAfterPurge(t *testing.T) {
	st := adoptionBoxes(t)
	ctx := context.Background()
	if err := st.SetSandboxDiscoboxAPI(ctx, "project-1", "a", false); err != nil {
		t.Fatal(err)
	}
	req := &model.APIAccessRequest{ProjectID: "project-1", RequesterSandboxID: "a", Justification: "Coordinate workers"}
	if err := st.CreateAPIAccessRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSandbox(ctx, "project-1", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DecideAPIAccessRequest(ctx, req.ProjectID, req.ID, "person", true); err == nil {
		t.Fatal("approved purged sandbox")
	}
	pending, err := st.GetAPIAccessRequest(ctx, req.ProjectID, req.ID)
	if err != nil || pending.Status != model.SecretRequestStatusPending {
		t.Fatal("failed approval changed request")
	}
	denied, err := st.DecideAPIAccessRequest(ctx, req.ProjectID, req.ID, "person", false)
	if err != nil || denied.Status != model.SecretRequestStatusDenied {
		t.Fatalf("denial failed: %#v %v", denied, err)
	}
}
