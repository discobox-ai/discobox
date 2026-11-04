package store_test

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func adoptionBoxes(t *testing.T) *store.Store {
	t.Helper()
	st := newTestStore(t)
	createTestPool(t, st, "project-1", "pool-1")
	for _, id := range []string{"a", "b", "child"} {
		sb := &model.Sandbox{ID: id, Name: id, ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", DiscoboxAPI: true}
		sb.DesiredState = model.DesiredStatePresent
		sb.State = model.SandboxStateReady
		if err := st.CreateSandbox(context.Background(), sb); err != nil {
			t.Fatal(err)
		}
	}
	return st
}
func askAdoption(t *testing.T, st *store.Store, parent, child string) *model.AdoptionRequest {
	t.Helper()
	req := &model.AdoptionRequest{ProjectID: "project-1", RequesterSandboxID: parent, ChildSandboxID: child, Justification: "lead the work", Status: model.SecretRequestStatusPending}
	if err := st.CreateAdoptionRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	return req
}
func TestAdoptionMovesOnlyParentAndRecordsDecisionAndEvents(t *testing.T) {
	st := adoptionBoxes(t)
	ctx := context.Background()
	req := askAdoption(t, st, "a", "child")
	if again := askAdoption(t, st, "a", "child"); again.ID != req.ID {
		t.Fatal("duplicate pending request")
	}
	approved, err := st.DecideAdoptionRequest(ctx, "project-1", req.ID, "person", true)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != "approved" || approved.DecidedBy != "person" || approved.DecidedAt == nil {
		t.Fatalf("decision not audited: %+v", approved)
	}
	stale, err := st.GetSandbox(ctx, "project-1", "child")
	if err != nil {
		t.Fatal(err)
	}
	if stale.ParentSandboxID == nil || *stale.ParentSandboxID != "a" || stale.CreatedBySandboxID != nil {
		t.Fatal("adoption changed creator or lost parent")
	}
	next := askAdoption(t, st, "b", "child")
	moved, err := st.DecideAdoptionRequest(ctx, "project-1", next.ID, "person", true)
	if err != nil {
		t.Fatal(err)
	}
	if moved.ReplacedParentSandboxID == nil || *moved.ReplacedParentSandboxID != "a" {
		t.Fatal("old parent not recorded")
	}
	// A stale whole-row save cannot resurrect the old lead or change creation history.
	creator := "b"
	stale.CreatedBySandboxID = &creator
	if err := st.UpdateSandbox(ctx, stale); err != nil {
		t.Fatal(err)
	}
	child, _ := st.GetSandbox(ctx, "project-1", "child")
	if child.ParentSandboxID == nil || *child.ParentSandboxID != "b" || child.CreatedBySandboxID != nil {
		t.Fatal("whole-row save overwrote authority or audit")
	}
	if err := st.ReleaseSandbox(ctx, "project-1", "child", "a"); err == nil {
		t.Fatal("old parent released child")
	}
	if err := st.ReleaseSandbox(ctx, "project-1", "child", "b"); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseSandbox(ctx, "project-1", "child", ""); err == nil {
		t.Fatal("released a box with no parent")
	}
	events := sandboxEvents(t, st, 0)
	var parents []string
	for _, event := range events {
		data := eventData(t, event)
		if data.SandboxID == "child" {
			parents = append(parents, data.ParentSandboxID)
		}
	}
	if len(parents) != 4 || parents[1] != "a" || parents[2] != "b" || parents[3] != "" {
		t.Fatalf("parent events: %v", parents)
	}
}

func TestCrossingAdoptionApprovalsCannotCreateACycle(t *testing.T) {
	st := adoptionBoxes(t)
	ctx := context.Background()
	ab, ba := askAdoption(t, st, "a", "b"), askAdoption(t, st, "b", "a")
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, req := range []*model.AdoptionRequest{ab, ba} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := st.DecideAdoptionRequest(ctx, "project-1", req.ID, "person", true)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("%d approvals succeeded", successes)
	}
	pending, err := st.ListAdoptionRequests(ctx, "project-1", "pending", "")
	if err != nil || len(pending) != 1 {
		t.Fatalf("failed approval did not stay pending: %v %v", pending, err)
	}
}

func TestAdoptionRechecksLimitsAndPurgeClearsChildren(t *testing.T) {
	for _, change := range []string{"self", "ancestor", "no-api", "archived", "deleting", "missing", "cross-project", "already-parent"} {
		t.Run(change, func(t *testing.T) {
			st := adoptionBoxes(t)
			ctx := context.Background()
			req := askAdoption(t, st, "a", "child")
			switch change {
			case "self":
				req.ChildSandboxID = "a"
			case "ancestor":
				other := askAdoption(t, st, "child", "a")
				if _, err := st.DecideAdoptionRequest(ctx, "project-1", other.ID, "person", true); err != nil {
					t.Fatal(err)
				}
			case "no-api":
				if err := st.SetSandboxDiscoboxAPI(ctx, "project-1", "a", false); err != nil {
					t.Fatal(err)
				}
			case "archived", "deleting":
				child, _ := st.GetSandbox(ctx, "project-1", "child")
				if change == "archived" {
					child.DesiredState = model.DesiredStateArchived
				} else {
					child.DesiredState = model.DesiredStateDeleted
				}
				if err := st.UpdateSandbox(ctx, child); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := st.DeleteSandbox(ctx, "project-1", "a"); err != nil {
					t.Fatal(err)
				}
			case "cross-project":
				req.ProjectID = "another-project"
			case "already-parent":
				other := *req
				other.ID = ""
				if err := st.CreateAdoptionRequest(ctx, &other); err != nil {
					t.Fatal(err)
				}
				if _, err := st.DecideAdoptionRequest(ctx, "project-1", other.ID, "person", true); err != nil {
					t.Fatal(err)
				}
			}
			// Mutations of the named IDs exercise filing; mutable state exercises approval too.
			if err := st.CreateAdoptionRequest(ctx, req); err == nil {
				t.Fatal("invalid filing allowed")
			}
			if change != "self" && change != "cross-project" && change != "already-parent" {
				if _, err := st.DecideAdoptionRequest(ctx, "project-1", req.ID, "person", true); err == nil {
					t.Fatal("invalid approval allowed")
				}
			}
		})
	}
	st := adoptionBoxes(t)
	ctx := context.Background()
	req := askAdoption(t, st, "a", "child")
	if _, err := st.DecideAdoptionRequest(ctx, "project-1", req.ID, "person", true); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSandbox(ctx, "project-1", "a"); err != nil {
		t.Fatal(err)
	}
	child, _ := st.GetSandbox(ctx, "project-1", "child")
	if child.ParentSandboxID != nil {
		t.Fatal("purge left dangling parent")
	}
	audited, err := st.GetAdoptionRequest(ctx, "project-1", req.ID)
	if err != nil || audited.Status != "approved" {
		t.Fatal("purge lost adoption audit")
	}
}

func TestCreateAndPurgeCannotLeaveADanglingParent(t *testing.T) {
	st := adoptionBoxes(t)
	ctx := context.Background()
	start := make(chan struct{})
	done := make(chan error, 2)
	parent := "a"
	child := &model.Sandbox{ID: "racing-child", Name: "racing-child", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", CreatedBySandboxID: &parent}
	go func() { <-start; done <- st.CreateSandbox(ctx, child) }()
	go func() { <-start; done <- st.DeleteSandbox(ctx, "project-1", parent) }()
	close(start)
	<-done
	<-done
	persisted, err := st.GetSandbox(ctx, "project-1", child.ID)
	if err == nil && persisted.ParentSandboxID != nil {
		t.Fatal("create raced purge and left a dangling parent")
	}
	late := *child
	late.ID, late.Name = "late-child", "late-child"
	if err := st.CreateSandbox(ctx, &late); err == nil {
		t.Fatal("creation with a purged creator succeeded")
	}
}

func TestAdoptionRecordsBelongToTheirProject(t *testing.T) {
	st := adoptionBoxes(t)
	ctx := context.Background()
	req := askAdoption(t, st, "a", "child")
	if _, err := st.DecideAdoptionRequest(ctx, "project-1", req.ID, "person", true); err != nil {
		t.Fatal(err)
	}
	children, err := st.ListSandboxes(ctx, "project-1", "", nil, "a")
	if err != nil || len(children) != 1 || children[0].ID != "child" {
		t.Fatalf("parent filter: %v %v", children, err)
	}
	for _, id := range []string{"a", "b", "child"} {
		if err := st.DeleteSandbox(ctx, "project-1", id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.GetAdoptionRequest(ctx, "project-1", req.ID); err != nil {
		t.Fatal("sandbox purge lost audit")
	}
	if err := st.DeletePool(ctx, "project-1", "pool-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteProject(ctx, "project-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAdoptionRequest(ctx, "project-1", req.ID); err == nil {
		t.Fatal("project deletion left adoption records")
	}
}

func TestAdoptionAndReleasePreserveCreationPassthrough(t *testing.T) {
	for _, createdByBox := range []bool{false, true} {
		st := adoptionBoxes(t)
		ctx := context.Background()
		if err := st.SetProjectEgress(ctx, "project-1", egress.Policy{Passthrough: []string{"internal.example"}}); err != nil {
			t.Fatal(err)
		}
		childID := "child"
		if createdByBox {
			creator := "a"
			child := &model.Sandbox{ID: "made-child", Name: "made-child", ProjectID: "project-1", PoolID: "pool-1", CreatedByUserID: "user-1", CreatedBySandboxID: &creator}
			if err := st.CreateSandbox(ctx, child); err != nil {
				t.Fatal(err)
			}
			childID = child.ID
		}
		child, _ := st.GetSandbox(ctx, "project-1", childID)
		before, err := st.EffectiveSandboxEgress(ctx, child)
		if err != nil {
			t.Fatal(err)
		}
		req := askAdoption(t, st, "b", childID)
		if _, err := st.DecideAdoptionRequest(ctx, "project-1", req.ID, "person", true); err != nil {
			t.Fatal(err)
		}
		child, _ = st.GetSandbox(ctx, "project-1", childID)
		after, err := st.EffectiveSandboxEgress(ctx, child)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("adoption changed policy: %v -> %v (%v)", before, after, err)
		}
		if err := st.ReleaseSandbox(ctx, "project-1", childID, ""); err != nil {
			t.Fatal(err)
		}
		child, _ = st.GetSandbox(ctx, "project-1", childID)
		after, err = st.EffectiveSandboxEgress(ctx, child)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("release changed policy: %v -> %v (%v)", before, after, err)
		}
		if createdByBox != (child.CreatedBySandboxID != nil) {
			t.Fatal("creation history changed")
		}
	}
}
