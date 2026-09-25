package secrets_test

import (
	"context"
	"testing"
	"time"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// staleToken is a token with a refresh command whose value went stale an hour
// ago, granted to and bound in one discobox under sentinel.
func staleToken(t *testing.T, st *store.Store, name, sandboxID, sentinel string) *model.Secret {
	t.Helper()
	sec := &model.Secret{
		ProjectID: "project-1", Name: name, Type: model.SecretTypeToken,
		MaxGrantTTL: 3600, EncryptedValue: mustTokenValue(t, "old-"+name),
		TTL: 300, RefreshCommand: []string{"gh", "auth", "token"},
	}
	sec.ValueWritten(time.Now().UTC().Add(-time.Hour), nil)
	if err := st.CreateSecret(context.Background(), sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	mustGrant(t, st, sec.ID, model.SecretGrantScopeSandbox, sandboxID)
	if err := st.CreateSandboxSecret(context.Background(), &model.SandboxSecret{
		ProjectID: "project-1", SandboxID: sandboxID, SecretID: sec.ID, EnvName: "TOKEN_" + name, Sentinel: sentinel,
	}); err != nil {
		t.Fatalf("create assignment: %v", err)
	}
	return sec
}

// The refresh trail is each ask and how it closed, as separate events in
// time, named by the secret, never carrying a value; it reads back and
// forward, bounded, and by discobox.
func TestRefreshTrailRecordsEachAskAndItsAnswer(t *testing.T) {
	svc, st, db := newResolveFixtureDB(t)
	ctx := context.Background()
	createSandbox(t, st, "sb-1", "pool-1")
	createSandbox(t, st, "sb-2", "pool-1")
	answered := staleToken(t, st, "github", "sb-1", "SENTINEL-A")
	dismissed := staleToken(t, st, "gcloud", "sb-2", "SENTINEL-B")

	if _, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-1", "SENTINEL-A", "github.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveSandboxSecret(ctx, "pool-1", "sb-2", "SENTINEL-B", "example.com"); err != nil {
		t.Fatal(err)
	}
	openA, err := st.FindPendingRefreshRequest(ctx, "project-1", answered.ID)
	if err != nil {
		t.Fatal(err)
	}
	openB, err := st.FindPendingRefreshRequest(ctx, "project-1", dismissed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RefreshSecret(testPrincipalContext(), "project-1", answered.ID, services.RefreshSecretBody{
		Value:      "new-github",
		Via:        apigen.RefreshSecretBodyViaCommand,
		Command:    apigen.NewOptNilStringArray([]string{"gh", "auth", "token"}),
		RequestId:  apigen.NewOptString(openA.ID),
		ClientHost: apigen.NewOptString("host-a"),
	}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if err := svc.DenySecretRequest(testPrincipalContext(), "project-1", openB.ID); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	// The flow above stamps every event within a millisecond. Seconds apart,
	// the order below is the order they happened in rather than a question
	// of fractional digits.
	base := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	for id, times := range map[string][2]time.Time{
		openA.ID: {base, base.Add(10 * time.Second)},
		openB.ID: {base.Add(5 * time.Second), base.Add(20 * time.Second)},
	} {
		if err := db.Model(&model.SecretRequest{}).Where("id = ?", id).
			Updates(map[string]any{"created_at": times[0], "closed_at": times[1]}).Error; err != nil {
			t.Fatal(err)
		}
	}

	events, err := svc.ListSecretRefreshEvents(ctx, "project-1", store.SecretRefreshFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("events = %+v, want two asks and two closings", events)
	}
	for i := 1; i < len(events); i++ {
		if events[i].At.After(events[i-1].At) {
			t.Fatalf("events not newest first: %+v", events)
		}
	}
	byKey := map[string]model.SecretRefreshEvent{}
	for _, e := range events {
		byKey[e.ID+"/"+e.Event] = e
	}
	answer := byKey[openA.ID+"/"+model.SecretRefreshEventAnswered]
	if answer.Answer == nil || answer.Answer.Via != model.SecretRefreshViaCommand || answer.Answer.ClientHost != "host-a" ||
		answer.Answer.AnsweredBy != "user-1" || answer.SecretName != "github" || answer.SandboxID != "sb-1" {
		t.Fatalf("answered event = %+v", answer)
	}
	if asked := byKey[openA.ID+"/"+model.SecretRefreshEventAsked]; asked.RefreshCause != model.SecretRefreshCauseStale || asked.Answer != nil {
		t.Fatalf("asked event = %+v", asked)
	}
	if _, ok := byKey[openB.ID+"/"+model.SecretRefreshEventDismissed]; !ok {
		t.Fatalf("events = %+v, want the dismissal", events)
	}

	// By discobox.
	onlyB, err := svc.ListSecretRefreshEvents(ctx, "project-1", store.SecretRefreshFilter{SandboxID: "sb-2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyB) != 2 || onlyB[0].ID != openB.ID || onlyB[1].ID != openB.ID {
		t.Fatalf("sb-2 events = %+v", onlyB)
	}

	// Forward, limited: the oldest events, oldest first.
	forward, err := svc.ListSecretRefreshEvents(ctx, "project-1", store.SecretRefreshFilter{Ascending: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(forward) != 2 || forward[0].Event != model.SecretRefreshEventAsked || forward[1].Event != model.SecretRefreshEventAsked {
		t.Fatalf("forward = %+v, want the two asks first", forward)
	}
	// Back, limited: the newest event only.
	back, err := svc.ListSecretRefreshEvents(ctx, "project-1", store.SecretRefreshFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 || back[0].Event != model.SecretRefreshEventDismissed {
		t.Fatalf("back = %+v, want the dismissal, the last thing that happened", back)
	}
	// A bound after the asks keeps only the closings: an ask made before
	// Since contributes its answer.
	after := forward[1].At.Add(time.Nanosecond)
	closings, err := svc.ListSecretRefreshEvents(ctx, "project-1", store.SecretRefreshFilter{Since: after, Ascending: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range closings {
		if e.Event == model.SecretRefreshEventAsked {
			t.Fatalf("closings = %+v, want no asks after %v", closings, after)
		}
	}
	if len(closings) != 2 {
		t.Fatalf("closings = %+v, want both", closings)
	}
}
