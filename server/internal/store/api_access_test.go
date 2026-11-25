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

// A purged sandbox's request is closed by the purge, so a person can neither
// approve it, which would grant API access to nothing, nor answer it again.
func TestAPIAccessRequestOfAPurgedSandboxCannotBeAnswered(t *testing.T) {
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
	for _, approve := range []bool{true, false} {
		if _, err := st.DecideAPIAccessRequest(ctx, req.ProjectID, req.ID, "person", approve); err == nil {
			t.Fatalf("answered (approve=%v) the request of a purged sandbox", approve)
		}
	}
	got, err := st.GetAPIAccessRequest(ctx, req.ProjectID, req.ID)
	if err != nil || got.Status != model.SecretRequestStatusDenied || got.DecidedBy != model.SecretBindingActorSystem {
		t.Fatalf("request of a purged sandbox = %#v %v, want denied by system", got, err)
	}
}

// Purging a sandbox closes its open API access request as denied, as it does
// its secret requests (TestDeletingASandboxDeniesItsOpenRequests): the agent
// that asked is gone, so nothing will read the answer.
func TestPurgingASandboxDeniesItsOpenAPIAccessRequest(t *testing.T) {
	st := adoptionBoxes(t)
	ctx := context.Background()
	if err := st.SetSandboxDiscoboxAPI(ctx, "project-1", "a", false); err != nil {
		t.Fatal(err)
	}
	req := &model.APIAccessRequest{ProjectID: "project-1", RequesterSandboxID: "a", Justification: "Coordinate workers", Status: model.SecretRequestStatusPending}
	if err := st.CreateAPIAccessRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSandbox(ctx, "project-1", "a"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetAPIAccessRequest(ctx, "project-1", req.ID)
	if err != nil {
		t.Fatalf("purge lost the request: %v", err)
	}
	if got.Status != model.SecretRequestStatusDenied || got.DecidedAt == nil || got.DecidedBy != model.SecretBindingActorSystem {
		t.Fatalf("API access request of a purged sandbox = %s by %q, want denied by %q", got.Status, got.DecidedBy, model.SecretBindingActorSystem)
	}
}
