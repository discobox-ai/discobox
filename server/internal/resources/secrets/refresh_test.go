package secrets_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"gorm.io/gorm"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// renewableFixture is a token with a lifetime and a command, granted to one
// discobox, its value written at writtenAt.
func renewableFixture(t *testing.T, writtenAt time.Time) (*resourcesecrets.Service, *store.Store, *gorm.DB, *model.Secret) {
	t.Helper()
	svc, st, db := newResolveFixtureDB(t)
	createSandbox(t, st, "sb-1", "pool-1")
	sec := &model.Secret{
		ProjectID: "project-1", Name: "github", Type: model.SecretTypeToken, Host: "github.com",
		MaxGrantTTL: 3600, EncryptedValue: mustTokenValue(t, "gho_old"),
		TTL: 300, RefreshCommand: []string{"gh", "auth", "token"},
	}
	sec.ValueWritten(writtenAt, nil)
	if err := st.CreateSecret(context.Background(), sec); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	mustGrant(t, st, sec.ID, model.SecretGrantScopeSandbox, "sb-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-A")
	return svc, st, db, sec
}

func pendingRefresh(t *testing.T, st *store.Store, secretID string) *model.SecretRequest {
	t.Helper()
	req, err := st.FindPendingRefreshRequest(context.Background(), "project-1", secretID)
	if err != nil {
		return nil
	}
	return req
}

// A fresh value is served with the proxy's cache capped at when it goes stale,
// and asks nobody for anything.
func TestResolveServesAFreshRenewableValueUntilItGoesStale(t *testing.T) {
	written := time.Now().UTC().Add(-time.Minute)
	svc, st, _, sec := renewableFixture(t, written)

	res, err := svc.ResolveSandboxSecret(context.Background(), "pool-1", "sb-1", "SENTINEL-A", "api.github.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Status != model.SecretRequestStatusApproved || res.Value.Token != "gho_old" {
		t.Fatalf("resolution = %+v, want the value on hand", res)
	}
	want := written.Add(300 * time.Second)
	if res.ExpiresAt == nil || !res.ExpiresAt.Equal(want) {
		t.Fatalf("expiresAt = %v, want the value's stale time %v", res.ExpiresAt, want)
	}
	if req := pendingRefresh(t, st, sec.ID); req != nil {
		t.Fatalf("a fresh value opened a refresh request: %+v", req)
	}
}

// A stale value is still served — the upstream decides whether it is too old —
// but briefly, and one refresh request stands for the secret however many
// resolves notice it.
func TestResolveServesAStaleValueAndAsksOnceForAnother(t *testing.T) {
	svc, st, _, sec := renewableFixture(t, time.Now().UTC().Add(-time.Hour))
	createSandbox(t, st, "sb-2", "pool-1")
	mustGrant(t, st, sec.ID, model.SecretGrantScopeSandbox, "sb-2")
	if err := st.CreateSandboxSecret(context.Background(), &model.SandboxSecret{
		ProjectID: "project-1", SandboxID: "sb-2", SecretID: sec.ID, EnvName: "FOO", Sentinel: "SENTINEL-B",
	}); err != nil {
		t.Fatal(err)
	}

	before := time.Now()
	res, err := svc.ResolveSandboxSecret(context.Background(), "pool-1", "sb-1", "SENTINEL-A", "github.com")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if res.Status != model.SecretRequestStatusApproved || res.Value.Token != "gho_old" {
		t.Fatalf("resolution = %+v, want the stale value served", res)
	}
	if res.ExpiresAt == nil || res.ExpiresAt.Sub(before) > time.Minute {
		t.Fatalf("expiresAt = %v, want a short hold on a stale value", res.ExpiresAt)
	}
	first := pendingRefresh(t, st, sec.ID)
	if first == nil || !first.IsRefresh() || first.RefreshCause != model.SecretRefreshCauseStale || first.SandboxID != "sb-1" {
		t.Fatalf("refresh request = %+v, want one opened by sb-1 for staleness", first)
	}

	if _, err := svc.ResolveSandboxSecret(context.Background(), "pool-1", "sb-2", "SENTINEL-B", "github.com"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	all, err := st.ListSecretRequests(context.Background(), "project-1", model.SecretRequestStatusPending)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != first.ID || all[0].SandboxID != "sb-2" {
		t.Fatalf("pending = %+v, want the one request, now naming sb-2", all)
	}
}

// Writing the value answers the request: the first answer is recorded with how
// the client says it made the value, and a second answer to the same request
// writes nothing.
func TestRefreshAnswersTheRequestAndTheFirstAnswerWins(t *testing.T) {
	svc, st, _, sec := renewableFixture(t, time.Now().UTC().Add(-time.Hour))
	if _, err := svc.ResolveSandboxSecret(context.Background(), "pool-1", "sb-1", "SENTINEL-A", "github.com"); err != nil {
		t.Fatal(err)
	}
	req := pendingRefresh(t, st, sec.ID)
	if req == nil {
		t.Fatal("no refresh request opened")
	}

	ctx := testPrincipalContext()
	body := services.RefreshSecretBody{
		Value:      "gho_new",
		Via:        apigen.RefreshSecretBodyViaCommand,
		Command:    apigen.NewOptNilStringArray([]string{"gh", "auth", "token"}),
		RequestId:  apigen.NewOptString(req.ID),
		Session:    apigen.NewOptBool(true),
		ClientHost: apigen.NewOptString("host-a"),
	}
	got, err := svc.RefreshSecret(ctx, "project-1", sec.ID, body)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got.StaleAt == nil || time.Until(*got.StaleAt) < 4*time.Minute {
		t.Fatalf("staleAt = %v, want a fresh lifetime", got.StaleAt)
	}

	res, err := svc.ResolveSandboxSecret(context.Background(), "pool-1", "sb-1", "SENTINEL-A", "github.com")
	if err != nil || res.Value.Token != "gho_new" {
		t.Fatalf("resolve after refresh = %+v, %v; want the new value", res, err)
	}
	answered, err := st.GetSecretRequest(context.Background(), "project-1", req.ID)
	if err != nil {
		t.Fatal(err)
	}
	a := answered.RefreshAnswer
	if answered.Status != model.SecretRequestStatusApproved || a == nil ||
		a.Via != model.SecretRefreshViaCommand || a.AnsweredBy != "user-1" || !a.Session || a.ClientHost != "host-a" ||
		a.CommandDigest != resourcesecrets.RefreshCommandDigest([]string{"gh", "auth", "token"}) {
		t.Fatalf("answered request = %+v / %+v", answered, a)
	}

	body.Value = "gho_late"
	_, err = svc.RefreshSecret(ctx, "project-1", sec.ID, body)
	if statusOf(err) != http.StatusConflict {
		t.Fatalf("second answer err = %v, want 409", err)
	}
	res, _ = svc.ResolveSandboxSecret(context.Background(), "pool-1", "sb-1", "SENTINEL-A", "github.com")
	if res.Value.Token != "gho_new" {
		t.Fatalf("value = %q after a refused second answer, want the first answer's", res.Value.Token)
	}
}

// An ordinary value write renews the token just as well, and closes the
// request as such.
func TestUpdatingTheValueClosesTheRefreshRequest(t *testing.T) {
	svc, st, _, sec := renewableFixture(t, time.Now().UTC().Add(-time.Hour))
	if _, err := svc.ResolveSandboxSecret(context.Background(), "pool-1", "sb-1", "SENTINEL-A", "github.com"); err != nil {
		t.Fatal(err)
	}
	req := pendingRefresh(t, st, sec.ID)
	value := apigen.SecretValue{Token: apigen.NewOptString("gho_pasted")}
	updated, err := svc.UpdateSecret(testPrincipalContext(), "project-1", sec.ID, services.UpdateSecretBody{Value: apigen.NewOptSecretValue(value)})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.StaleAt == nil || !updated.StaleAt.After(time.Now()) {
		t.Fatalf("staleAt = %v, want the new value's lifetime", updated.StaleAt)
	}
	closed, err := st.GetSecretRequest(context.Background(), "project-1", req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != model.SecretRequestStatusApproved || closed.RefreshAnswer == nil || closed.RefreshAnswer.Via != model.SecretRefreshViaUpdate {
		t.Fatalf("request = %+v, want it answered by the update", closed)
	}
}

// A refresh request is answered with a value: it cannot be approved, and a
// discobox may neither dismiss it nor write the value.
func TestARefreshRequestIsAPersonsToAnswer(t *testing.T) {
	svc, st, _, sec := renewableFixture(t, time.Now().UTC().Add(-time.Hour))
	if _, err := svc.ResolveSandboxSecret(context.Background(), "pool-1", "sb-1", "SENTINEL-A", "github.com"); err != nil {
		t.Fatal(err)
	}
	req := pendingRefresh(t, st, sec.ID)

	if _, err := svc.ApproveSecretRequest(testPrincipalContext(), "project-1", req.ID, services.ApproveSecretRequestBody{SecretId: apigen.NewOptString(sec.ID)}); statusOf(err) != http.StatusBadRequest {
		t.Fatalf("approve err = %v, want 400", err)
	}
	asSandbox := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypeSandbox, SandboxID: "sb-1"})
	if err := svc.DenySecretRequest(asSandbox, "project-1", req.ID); statusOf(err) != http.StatusForbidden {
		t.Fatalf("deny as a discobox err = %v, want 403", err)
	}
	_, err := svc.RefreshSecret(asSandbox, "project-1", sec.ID, services.RefreshSecretBody{Value: "x", Via: apigen.RefreshSecretBodyViaEntered})
	if statusOf(err) != http.StatusForbidden {
		t.Fatalf("refresh as a discobox err = %v, want 403", err)
	}
	if err := svc.DenySecretRequest(testPrincipalContext(), "project-1", req.ID); err != nil {
		t.Fatalf("dismiss as a person: %v", err)
	}
}

// A refused renewable token is not a dead end: nothing is recorded as a
// rejection, its value is stale from now, and a refresh request says why.
func TestARefusedRenewableTokenAsksForANewValue(t *testing.T) {
	svc, st, _, sec := renewableFixture(t, time.Now().UTC().Add(-2*time.Minute))
	err := svc.RecordSandboxSecretRejection(context.Background(), "pool-1", "sb-1", "SENTINEL-A", "github.com", resourcesecrets.SecretRejectedOutcome, "")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	rejections, err := svc.ListSecretRejections(context.Background(), "project-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rejections) != 0 {
		t.Fatalf("rejections = %+v, want none for a token a client renews", rejections)
	}
	req := pendingRefresh(t, st, sec.ID)
	if req == nil || req.RefreshCause != model.SecretRefreshCauseRejected {
		t.Fatalf("refresh request = %+v, want one opened by the rejection", req)
	}
	got, err := svc.GetSecret(context.Background(), "project-1", sec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StaleAt == nil || got.StaleAt.After(time.Now()) {
		t.Fatalf("staleAt = %v, want stale now", got.StaleAt)
	}
}

// A lifetime and a command belong to a token, a command names a program, and
// naming a command without a lifetime gives the default one.
func TestCreateSecretLifetime(t *testing.T) {
	svc := newTestService(t)
	ctx := testPrincipalContext()
	body := services.CreateSecretBody{
		Name:           "github",
		Type:           apigen.CreateSecretBodyTypeToken,
		Value:          apigen.SecretValue{Token: apigen.NewOptString("gho_x")},
		RefreshCommand: apigen.NewOptNilStringArray([]string{"gh", "auth", "token"}),
	}
	sec, err := svc.CreateSecret(ctx, "project-1", body)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sec.TTL != model.DefaultRefreshTTL || sec.StaleAt == nil {
		t.Fatalf("secret = %+v, want the default lifetime", sec)
	}

	body.Name = "blank"
	body.RefreshCommand = apigen.NewOptNilStringArray([]string{" ", "x"})
	if _, err := svc.CreateSecret(ctx, "project-1", body); statusOf(err) != http.StatusBadRequest {
		t.Fatalf("blank program err = %v, want 400", err)
	}

	body.Name = "oauth"
	body.Type = apigen.CreateSecretBodyTypeOAuth
	body.Value = apigen.SecretValue{Token: apigen.NewOptString("a"), RefreshToken: apigen.NewOptString("r"), TokenUrl: apigen.NewOptString("https://x/token")}
	body.RefreshCommand = apigen.NewOptNilStringArray(nil)
	body.TtlSeconds = apigen.NewOptInt64(60)
	if _, err := svc.CreateSecret(ctx, "project-1", body); statusOf(err) != http.StatusBadRequest {
		t.Fatalf("oauth with a lifetime err = %v, want 400", err)
	}
}

func statusOf(err error) int {
	var status interface{ StatusCode() int }
	if errors.As(err, &status) {
		return status.StatusCode()
	}
	return 0
}

// An open refresh request is not the reactive ask: a discobox with no grant
// on the same token still gets an ask a person can approve.
func TestARefreshRequestIsNotTheAskForAGrant(t *testing.T) {
	svc, st, _, sec := renewableFixture(t, time.Now().UTC().Add(-time.Hour))
	if _, err := svc.ResolveSandboxSecret(context.Background(), "pool-1", "sb-1", "SENTINEL-A", "github.com"); err != nil {
		t.Fatal(err)
	}
	createSandbox(t, st, "sb-2", "pool-1")
	if err := st.CreateSandboxSecret(context.Background(), &model.SandboxSecret{
		ProjectID: "project-1", SandboxID: "sb-2", SecretID: sec.ID, EnvName: "FOO", Sentinel: "SENTINEL-B",
	}); err != nil {
		t.Fatal(err)
	}
	// The refresh request names sb-2 once sb-2 needs the value; then sb-2,
	// with no grant, resolves.
	if err := st.SetRefreshRequestSandbox(context.Background(), "project-1", pendingRefresh(t, st, sec.ID).ID, "sb-2"); err != nil {
		t.Fatal(err)
	}
	res, err := svc.ResolveSandboxSecret(context.Background(), "pool-1", "sb-2", "SENTINEL-B", "github.com")
	if err != nil || res.Status != model.SecretRequestStatusPending {
		t.Fatalf("resolve = %+v, %v; want pending", res, err)
	}
	pending, err := st.ListSecretRequests(context.Background(), "project-1", model.SecretRequestStatusPending)
	if err != nil {
		t.Fatal(err)
	}
	var asks int
	for _, req := range pending {
		if !req.IsRefresh() && req.SandboxID == "sb-2" {
			asks++
		}
	}
	if asks != 1 {
		t.Fatalf("pending = %+v, want one ask for a grant from sb-2 beside the refresh request", pending)
	}
}

// A token created for a well-known ID answers it from the start, and the ID
// is one secret's: a second is refused, as is a gate.
func TestASecretCanBeCreatedForAWellKnownID(t *testing.T) {
	svc := newTestService(t)
	ctx := testPrincipalContext()
	body := services.CreateSecretBody{
		Name:        "github",
		Type:        apigen.CreateSecretBodyTypeToken,
		Value:       apigen.SecretValue{Token: apigen.NewOptString("gho_x")},
		WellKnownId: apigen.NewOptString("com.github.api"),
	}
	sec, err := svc.CreateSecret(ctx, "project-1", body)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sec.WellKnownID != "com.github.api" {
		t.Fatalf("wellKnownId = %q, want the ID it was created for", sec.WellKnownID)
	}
	// Created with no command, it has no lifetime; one created with gh's
	// command and none named takes the lifetime GitHub's token really has.
	if sec.TTL != 0 {
		t.Fatalf("ttl = %d, want none without a command", sec.TTL)
	}
	if err := svc.DeleteSecret(ctx, "project-1", sec.ID); err != nil {
		t.Fatal(err)
	}
	body.RefreshCommand = apigen.NewOptNilStringArray([]string{"gh", "auth", "token"})
	sec, err = svc.CreateSecret(ctx, "project-1", body)
	if err != nil {
		t.Fatalf("create with command: %v", err)
	}
	if sec.TTL != 24*60*60 {
		t.Fatalf("ttl = %d, want GitHub's day", sec.TTL)
	}
	body.Name = "github-2"
	if _, err := svc.CreateSecret(ctx, "project-1", body); statusOf(err) != http.StatusConflict {
		t.Fatalf("second secret for the ID err = %v, want 409", err)
	}
	body.Name = "gate"
	body.WellKnownId = apigen.NewOptString("ai.discobox.sandbox")
	if _, err := svc.CreateSecret(ctx, "project-1", body); statusOf(err) != http.StatusBadRequest {
		t.Fatalf("gate err = %v, want 400", err)
	}
}

// A rename written from a copy read before a rejection marked the value stale
// keeps the mark: the value's own fields are not the rename's to write back.
// And a value written a moment ago is not marked stale by a report that may be
// about the value it replaced.
func TestTheStaleMarkSurvivesAnEditAndSparesAFreshValue(t *testing.T) {
	svc, st, _, sec := renewableFixture(t, time.Now().UTC().Add(-2*time.Minute))
	ctx := context.Background()
	before, err := st.GetSecret(ctx, "project-1", sec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "github.com", resourcesecrets.SecretRejectedOutcome, ""); err != nil {
		t.Fatal(err)
	}
	before.Name = "github-renamed"
	if err := st.UpdateSecret(ctx, before); err != nil {
		t.Fatalf("rename: %v", err)
	}
	after, err := svc.GetSecret(ctx, "project-1", sec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.StaleAt == nil || after.StaleAt.After(time.Now()) {
		t.Fatalf("staleAt = %v after a rename, want the rejection's mark kept", after.StaleAt)
	}

	// Renewed now: a rejection report arriving next is about the old value.
	if _, err := svc.RefreshSecret(testPrincipalContext(), "project-1", sec.ID, services.RefreshSecretBody{Value: "gho_new", Via: apigen.RefreshSecretBodyViaEntered}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "github.com", resourcesecrets.SecretRejectedOutcome, ""); err != nil {
		t.Fatal(err)
	}
	fresh, _ := svc.GetSecret(ctx, "project-1", sec.ID)
	if fresh.StaleAt == nil || !fresh.StaleAt.After(time.Now()) {
		t.Fatalf("staleAt = %v, want the value just written left fresh", fresh.StaleAt)
	}
	if req := pendingRefresh(t, st, sec.ID); req != nil {
		t.Fatalf("refresh request = %+v, want none opened for the value just written", req)
	}
}

// One open refresh request per secret is the database's to hold, so two
// resolves racing past the find cannot both open one; a closed one does not
// count.
func TestOneOpenRefreshRequestPerSecretIsHeldByTheDatabase(t *testing.T) {
	_, st, _, sec := renewableFixture(t, time.Now().UTC().Add(-time.Hour))
	ctx := context.Background()
	open := func() error {
		return st.CreateSecretRequest(ctx, &model.SecretRequest{
			ProjectID: "project-1", RequestedBy: "sandbox:sb-1", SandboxID: "sb-1", Type: model.SecretTypeToken,
			SecretID: sec.ID, Status: model.SecretRequestStatusPending, Reason: model.SecretRequestReasonRefresh,
		})
	}
	if err := open(); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := open(); err == nil {
		t.Fatal("a second open refresh request for the secret was stored")
	}
	if err := st.AnswerOpenRefreshRequests(ctx, "project-1", sec.ID, &model.SecretRefreshAnswer{AnsweredAt: time.Now(), Via: model.SecretRefreshViaUpdate}); err != nil {
		t.Fatal(err)
	}
	if err := open(); err != nil {
		t.Fatalf("after the first was answered: %v", err)
	}
}

// A discobox lists secrets and requests for their names and bindings: the
// refresh command, and how a refresh was answered, are not in what it reads.
func TestADiscoboxDoesNotSeeTheRefreshCommandOrItsAnswers(t *testing.T) {
	svc, st, _, sec := renewableFixture(t, time.Now().UTC().Add(-time.Hour))
	if _, err := svc.ResolveSandboxSecret(context.Background(), "pool-1", "sb-1", "SENTINEL-A", "github.com"); err != nil {
		t.Fatal(err)
	}
	req := pendingRefresh(t, st, sec.ID)
	if _, err := svc.RefreshSecret(testPrincipalContext(), "project-1", sec.ID, services.RefreshSecretBody{
		Value: "gho_new", Via: apigen.RefreshSecretBodyViaCommand, Command: apigen.NewOptNilStringArray(ghCommandArgs), RequestId: apigen.NewOptString(req.ID),
	}); err != nil {
		t.Fatal(err)
	}
	asSandbox := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypeSandbox, SandboxID: "sb-1", ProjectID: "project-1"})

	secrets, err := svc.ListSecrets(asSandbox, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if len(s.RefreshCommand) > 0 {
			t.Fatalf("a discobox listed %s's refresh command", s.Name)
		}
	}
	got, err := svc.GetSecretRequest(asSandbox, "project-1", req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshAnswer != nil {
		t.Fatalf("a discobox read the refresh answer: %+v", got.RefreshAnswer)
	}
	// A person still sees both.
	mine, err := svc.GetSecret(testPrincipalContext(), "project-1", sec.ID)
	if err != nil || len(mine.RefreshCommand) == 0 {
		t.Fatalf("a person's read = %+v, %v; want the command", mine, err)
	}
}

var ghCommandArgs = []string{"gh", "auth", "token"}
