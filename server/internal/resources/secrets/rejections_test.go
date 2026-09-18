package secrets_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// An API key has nothing to renew, so the upstream's refusal is the whole
// story: what was stored is what was refused, and only a person can replace it.
func TestRecordRejectionOfATokenNeedsAPerson(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	sec := mustSecret(t, st, "gh", "dead-token")
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-A")

	err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "api.github.com", resourcesecrets.SecretRejectedOutcome, "")
	if err != nil {
		t.Fatalf("record rejection: %v", err)
	}

	got := mustRejection(t, st, sec.ID, "api.github.com")
	if got.Reason != model.SecretRejectionReasonUnrefreshable {
		t.Fatalf("reason = %q, want %q", got.Reason, model.SecretRejectionReasonUnrefreshable)
	}
	if got.SandboxID != "sb-1" {
		t.Fatalf("sandbox = %q, want the one that saw it", got.SandboxID)
	}
	if got.Count != 1 {
		t.Fatalf("count = %d, want 1", got.Count)
	}
}

// The recoverable case, and the reason the control plane refreshes before it
// judges: an access token can die before its stated expiry — a sign-out
// elsewhere, a plan change — while the refresh token behind it is perfectly
// good. Asking a person to sign in again for that is asking them to redo work
// the credential can do itself.
func TestRecordRejectionRenewsAnOAuthCredentialBeforeJudgingIt(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	tokenServer := newOAuthTokenServer(t, "rotated-access", "rotated-refresh", 3600)
	sec := mustOAuthSecret(t, st, "claude", model.SecretValue{
		Token:                "refused-access",
		RefreshToken:         "good-refresh",
		TokenURL:             tokenServer.server.URL,
		ClientID:             "client-1",
		AccessTokenExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	})
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-A")

	err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "api.anthropic.com", resourcesecrets.SecretRejectedOutcome, "")
	if err != nil {
		t.Fatalf("record rejection: %v", err)
	}

	if n := tokenServer.calls.Load(); n != 1 {
		t.Fatalf("token endpoint calls = %d, want one forced refresh", n)
	}
	if rejections := mustRejections(t, st); len(rejections) != 0 {
		t.Fatalf("rejections = %v, want none: the credential renewed", rejections)
	}
	// And the renewed credential is what the next request carries.
	fresh, err := st.GetSecret(ctx, "project-1", sec.ID)
	if err != nil {
		t.Fatalf("get secret: %v", err)
	}
	value, err := st.OpenSecretValue(ctx, fresh)
	if err != nil {
		t.Fatalf("open value: %v", err)
	}
	if value.Token != "rotated-access" {
		t.Fatalf("token = %q, want the rotated one", value.Token)
	}
}

// A refresh token the endpoint refuses is the end of the line: it is spent,
// revoked, or belongs to a session somebody ended somewhere else.
func TestRecordRejectionRecordsAFailedRenewal(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	t.Cleanup(refusing.Close)

	sec := mustOAuthSecret(t, st, "claude", model.SecretValue{
		Token:                "refused-access",
		RefreshToken:         "spent-refresh",
		TokenURL:             refusing.URL,
		ClientID:             "client-1",
		AccessTokenExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	})
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-A")

	err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "api.anthropic.com", resourcesecrets.SecretRejectedAfterRetryOutcome, "use-1")
	if err != nil {
		t.Fatalf("record rejection: %v", err)
	}

	got := mustRejection(t, st, sec.ID, "api.anthropic.com")
	if got.Reason != model.SecretRejectionReasonRefreshFailed {
		t.Fatalf("reason = %q, want %q", got.Reason, model.SecretRejectionReasonRefreshFailed)
	}
	if got.UseID != "use-1" {
		t.Fatalf("use = %q, want the use the sentinel was minted for", got.UseID)
	}
}

// A credential that renewed and was refused again is the one that costs the
// most certainty, and it is recorded as its own reason for that.
func TestRecordRejectionAfterASuccessfulRenewalSaysSo(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	// The endpoint hands back the same access token: a renewal that renewed
	// nothing, whatever it answered.
	tokenServer := newOAuthTokenServer(t, "refused-access", "rotated-refresh", 3600)
	sec := mustOAuthSecret(t, st, "claude", model.SecretValue{
		Token:                "refused-access",
		RefreshToken:         "good-refresh",
		TokenURL:             tokenServer.server.URL,
		ClientID:             "client-1",
		AccessTokenExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	})
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-A")

	err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "api.anthropic.com", resourcesecrets.SecretRejectedOutcome, "")
	if err != nil {
		t.Fatalf("record rejection: %v", err)
	}

	got := mustRejection(t, st, sec.ID, "api.anthropic.com")
	if got.Reason != model.SecretRejectionReasonRejectedAfterRefresh {
		t.Fatalf("reason = %q, want %q", got.Reason, model.SecretRejectionReasonRejectedAfterRefresh)
	}
}

// A harness credential fails in every sandbox on that harness at once, and each
// of those is a report. Without the cooldown a project with twenty boxes on a
// dead subscription would spend twenty refresh tokens a minute against an
// endpoint that has already said no.
func TestRecordRejectionRenewsOncePerCooldown(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	tokenServer := newOAuthTokenServer(t, "refused-access", "rotated-refresh", 3600)
	sec := mustOAuthSecret(t, st, "claude", model.SecretValue{
		Token:                "refused-access",
		RefreshToken:         "good-refresh",
		TokenURL:             tokenServer.server.URL,
		ClientID:             "client-1",
		AccessTokenExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	})
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	for _, sandbox := range []string{"sb-1", "sb-2", "sb-3"} {
		createSandbox(t, st, sandbox, "pool-1")
		mustAssign(t, st, sandbox, sec.ID, "SENTINEL-"+sandbox)
	}

	for _, sandbox := range []string{"sb-1", "sb-2", "sb-3"} {
		err := svc.RecordSandboxSecretRejection(ctx, "pool-1", sandbox, "SENTINEL-"+sandbox, "api.anthropic.com", resourcesecrets.SecretRejectedOutcome, "")
		if err != nil {
			t.Fatalf("record rejection from %s: %v", sandbox, err)
		}
	}

	if n := tokenServer.calls.Load(); n != 1 {
		t.Fatalf("token endpoint calls = %d, want one for three sandboxes on one credential", n)
	}
	got := mustRejection(t, st, sec.ID, "api.anthropic.com")
	if got.Count != 3 {
		t.Fatalf("count = %d, want all three sightings", got.Count)
	}
	if got.SandboxID != "sb-3" {
		t.Fatalf("sandbox = %q, want the most recent one", got.SandboxID)
	}
}

// A lapsed subscription can hold a perfectly good refresh token, and that is
// the quiet failure: every rejection renews happily, reads as recovered,
// records nothing, and is refused again a minute later — forever, spending a
// rotating refresh token each time, with nothing on screen to show for it. A
// credential refused again right after a renewal is one the renewal did not
// save.
func TestARenewalThatFixesNothingIsNotRecoveryTwice(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	// The endpoint rotates happily every time it is asked.
	var minted atomic.Int32
	rotating := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return
		}
		n := minted.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  fmt.Sprintf("rotated-%d", n),
			"refresh_token": fmt.Sprintf("refresh-%d", n),
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	}))
	t.Cleanup(rotating.Close)

	sec := mustOAuthSecret(t, st, "claude", model.SecretValue{
		Token:                "refused-access",
		RefreshToken:         "good-refresh",
		TokenURL:             rotating.URL,
		ClientID:             "client-1",
		AccessTokenExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	})
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-A")

	// The first rejection renews, and nothing is recorded: the new credential
	// deserves its chance.
	if err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "api.anthropic.com", resourcesecrets.SecretRejectedOutcome, ""); err != nil {
		t.Fatalf("record first rejection: %v", err)
	}
	if got := mustRejections(t, st); len(got) != 0 {
		t.Fatalf("rejections = %v, want none after a renewal", got)
	}

	// The renewed credential is refused too. That is the end of the renewing.
	if err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "api.anthropic.com", resourcesecrets.SecretRejectedOutcome, ""); err != nil {
		t.Fatalf("record second rejection: %v", err)
	}
	if n := minted.Load(); n != 1 {
		t.Fatalf("renewals = %d, want one: a second would have spent another refresh token for the same answer", n)
	}
	got := mustRejection(t, st, sec.ID, "api.anthropic.com")
	if got.Reason != model.SecretRejectionReasonRejectedAfterRefresh {
		t.Fatalf("reason = %q, want %q", got.Reason, model.SecretRejectionReasonRejectedAfterRefresh)
	}
}

// A credential is refused per destination. One that answers for one host and is
// refused at another is two different facts, and condemning the credential
// outright would be the wrong one.
func TestRecordRejectionIsPerHost(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	sec := mustSecret(t, st, "gh", "a-token")
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-A")

	for _, host := range []string{"api.github.com", "uploads.github.com"} {
		if err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", host, resourcesecrets.SecretRejectedOutcome, ""); err != nil {
			t.Fatalf("record rejection at %s: %v", host, err)
		}
	}
	if got := mustRejections(t, st); len(got) != 2 {
		t.Fatalf("rejections = %d, want one per host", len(got))
	}

	// And a clearance is per host too.
	if err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "api.github.com", resourcesecrets.SecretAcceptedOutcome, ""); err != nil {
		t.Fatalf("record acceptance: %v", err)
	}
	remaining := mustRejections(t, st)
	if len(remaining) != 1 || remaining[0].Host != "uploads.github.com" {
		t.Fatalf("rejections = %v, want only the host that is still refusing", remaining)
	}
}

// However the credential came to be fixed, the window must stop asking for a
// sign-in that has already happened.
func TestAcceptedOutcomeClearsARejection(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	sec := mustSecret(t, st, "gh", "dead-token")
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-A")

	if err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "api.github.com", resourcesecrets.SecretRejectedOutcome, ""); err != nil {
		t.Fatalf("record rejection: %v", err)
	}
	if err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "api.github.com", resourcesecrets.SecretAcceptedOutcome, ""); err != nil {
		t.Fatalf("record acceptance: %v", err)
	}
	if got := mustRejections(t, st); len(got) != 0 {
		t.Fatalf("rejections = %v, want none after the credential worked", got)
	}
}

// Replacing the value is the remedy, so it is also the retraction: nothing
// recorded about the credential that was refused is still true.
func TestReplacingASecretValueClearsItsRejections(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	sec := mustSecret(t, st, "gh", "dead-token")
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-A")
	if err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "api.github.com", resourcesecrets.SecretRejectedOutcome, ""); err != nil {
		t.Fatalf("record rejection: %v", err)
	}

	// A rename is not a replacement, and leaves the record standing.
	renamed := "github"
	if _, err := svc.UpdateSecret(ctx, "project-1", sec.ID, apimodel.UpdateSecretBody{
		Name: serverapi.NewOptString(renamed),
	}); err != nil {
		t.Fatalf("rename secret: %v", err)
	}
	if got := mustRejections(t, st); len(got) != 1 {
		t.Fatalf("rejections = %v, want the record to survive a rename", got)
	}

	value := apimodel.SecretValue{Token: serverapi.NewOptString("a-new-token")}
	if _, err := svc.UpdateSecret(ctx, "project-1", sec.ID, apimodel.UpdateSecretBody{
		Value: serverapi.NewOptSecretValue(value),
	}); err != nil {
		t.Fatalf("replace secret value: %v", err)
	}
	if got := mustRejections(t, st); len(got) != 0 {
		t.Fatalf("rejections = %v, want none after the value was replaced", got)
	}
}

// The listing has to say which credential was refused and what to do about it,
// which for a harness's own credential is configuring that harness again.
func TestListSecretRejectionsNamesTheHarnessThatOwnsTheCredential(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	cfg := &model.HarnessConfig{
		ID: "harness-1", ProjectID: "project-1", Name: "Claude Code", Slug: "claude-code", Configured: true,
	}
	if err := st.CreateHarnessConfig(ctx, cfg); err != nil {
		t.Fatalf("create harness config: %v", err)
	}
	sec := mustSecret(t, st, "claude-code oauth", "dead-token")
	cfg.ConfiguredSecretIDs = []string{sec.ID}
	if err := st.UpdateHarnessConfig(ctx, cfg); err != nil {
		t.Fatalf("update harness config: %v", err)
	}
	if err := st.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
		ProjectID: "project-1", HarnessConfigID: cfg.ID, EnvName: "ANTHROPIC_API_KEY", SecretID: sec.ID,
	}); err != nil {
		t.Fatalf("create binding: %v", err)
	}
	mustGrant(t, st, sec.ID, model.SecretGrantScopeHarnessConfig, cfg.ID)
	createSandboxWithHarness(t, st, "sb-1", "pool-1", cfg.ID)
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-A")

	if err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "api.anthropic.com", resourcesecrets.SecretRejectedOutcome, ""); err != nil {
		t.Fatalf("record rejection: %v", err)
	}

	listed, err := svc.ListSecretRejections(ctx, "project-1")
	if err != nil {
		t.Fatalf("list rejections: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("rejections = %d, want 1", len(listed))
	}
	got := listed[0]
	if got.HarnessConfigID != cfg.ID || got.HarnessConfigName != "Claude Code" {
		t.Fatalf("harness = %q/%q, want the config whose configure flow made the credential", got.HarnessConfigID, got.HarnessConfigName)
	}
	if got.EnvName != "ANTHROPIC_API_KEY" {
		t.Fatalf("envName = %q, want the variable it is delivered in", got.EnvName)
	}
	if got.SecretName != "claude-code oauth" || got.SecretType != model.SecretTypeToken {
		t.Fatalf("secret = %q/%q, want the refused credential named", got.SecretName, got.SecretType)
	}
}

// A report about a sandbox in another pool is not this pool's to make, the same
// rule resolving one follows.
func TestRecordRejectionRefusesAnotherPoolsSandbox(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	sec := mustSecret(t, st, "gh", "a-token")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-A")

	err := svc.RecordSandboxSecretRejection(ctx, "pool-2", "sb-1", "SENTINEL-A", "api.github.com", resourcesecrets.SecretRejectedOutcome, "")
	if err == nil {
		t.Fatal("record rejection from the wrong pool succeeded, want a refusal")
	}
	if got := mustRejections(t, st); len(got) != 0 {
		t.Fatalf("rejections = %v, want none", got)
	}
}

func TestRecordRejectionRefusesAnUnknownOutcome(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	sec := mustSecret(t, st, "gh", "a-token")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-A")

	if err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "api.github.com", "shrugged", ""); err == nil {
		t.Fatal("unknown outcome was accepted, want a refusal")
	}
}

func mustRejection(t *testing.T, st *store.Store, secretID, host string) model.SecretRejection {
	t.Helper()
	got, err := st.GetSecretRejection(context.Background(), "project-1", secretID, host)
	if err != nil {
		t.Fatalf("get rejection: %v", err)
	}
	if got == nil {
		t.Fatalf("no rejection recorded for %s at %s", secretID, host)
	}
	return *got
}

func mustRejections(t *testing.T, st *store.Store) []model.SecretRejection {
	t.Helper()
	got, err := st.ListSecretRejections(context.Background(), "project-1")
	if err != nil {
		t.Fatalf("list rejections: %v", err)
	}
	return got
}

// A token endpoint having a bad day says nothing about anybody's credential. If
// a 5xx or an unreachable host were recorded as "renewal refused", the band
// would ask a person to redo a sign-in they do not need — and the standing row
// would then suppress the renewal that was about to work.
func TestATokenEndpointOutageIsNotAVerdict(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	var calls atomic.Int32
	erroring := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return
		}
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(erroring.Close)

	sec := mustOAuthSecret(t, st, "claude", model.SecretValue{
		Token:                "refused-access",
		RefreshToken:         "good-refresh",
		TokenURL:             erroring.URL,
		ClientID:             "client-1",
		AccessTokenExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	})
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-A")

	if err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "api.anthropic.com", resourcesecrets.SecretRejectedOutcome, ""); err != nil {
		t.Fatalf("record rejection: %v", err)
	}
	if got := mustRejections(t, st); len(got) != 0 {
		t.Fatalf("rejections = %v, want none: the endpoint was down, the credential was not judged", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("token endpoint calls = %d, want the renewal to have been attempted", n)
	}

	// And nothing is suppressed: the next report tries the renewal again, which
	// is what makes this recoverable rather than sticky.
	if err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "api.anthropic.com", resourcesecrets.SecretRejectedOutcome, ""); err != nil {
		t.Fatalf("record second rejection: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("token endpoint calls = %d, want the renewal retried rather than suppressed", n)
	}
}

// The endpoint's own refusal is the one refresh failure that is about the
// credential, and it is what a person has to act on.
func TestARefusedRenewalIsAVerdict(t *testing.T) {
	ctx := context.Background()
	svc, st := newResolveFixture(t)

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	t.Cleanup(refusing.Close)

	sec := mustOAuthSecret(t, st, "claude", model.SecretValue{
		Token:                "refused-access",
		RefreshToken:         "spent-refresh",
		TokenURL:             refusing.URL,
		ClientID:             "client-1",
		AccessTokenExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	})
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-A")

	if err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "api.anthropic.com", resourcesecrets.SecretRejectedOutcome, ""); err != nil {
		t.Fatalf("record rejection: %v", err)
	}
	got := mustRejection(t, st, sec.ID, "api.anthropic.com")
	if got.Reason != model.SecretRejectionReasonRefreshFailed {
		t.Fatalf("reason = %q, want %q", got.Reason, model.SecretRejectionReasonRefreshFailed)
	}
}

// A clearance from the proxy only arrives while the process that reported the
// rejection is still running and still remembers it. Restart the pool agent, or
// delete the discobox that hit the failure, and a credential fixed upstream
// would keep a band nobody can dismiss — so a rejection nothing has observed
// lately is dropped on the way out.
func TestARejectionNothingHasSeenLatelyIsDropped(t *testing.T) {
	ctx := context.Background()
	svc, st, write := newResolveFixtureDB(t)

	fresh := mustSecret(t, st, "gh", "dead-token")
	stale := mustSecret(t, st, "npm", "old-token")
	for _, sec := range []*model.Secret{fresh, stale} {
		if err := st.RecordSecretRejection(ctx, &model.SecretRejection{
			ProjectID: "project-1", SecretID: sec.ID, Host: "api.example.com",
			Reason: model.SecretRejectionReasonUnrefreshable, SandboxID: "sb-1",
		}); err != nil {
			t.Fatalf("record rejection: %v", err)
		}
	}
	// One of them was last seen long enough ago that nothing is failing on it.
	if err := write.WithContext(ctx).Model(&model.SecretRejection{}).
		Where("secret_id = ?", stale.ID).
		Update("last_seen_at", time.Now().Add(-2*time.Hour)).Error; err != nil {
		t.Fatalf("age the rejection: %v", err)
	}

	listed, err := svc.ListSecretRejections(ctx, "project-1")
	if err != nil {
		t.Fatalf("list rejections: %v", err)
	}
	if len(listed) != 1 || listed[0].SecretID != fresh.ID {
		t.Fatalf("rejections = %v, want only the one still being refused", listed)
	}
	// And it is gone, not merely hidden: the next read costs nothing, and the
	// next failure raises it again.
	if got := mustRejections(t, st); len(got) != 1 {
		t.Fatalf("stored rejections = %v, want the stale one deleted", got)
	}
}

// A standing rejection is kept alive by being seen, not by being re-judged.
// When the renewal cannot be judged at all — a token-endpoint outage outlasting
// the judge cooldown — the reports still arriving are the evidence the
// credential is still being refused, and without recording them the row ages
// out mid-incident and the band disappears while the failure continues.
func TestAStandingRejectionSurvivesAnUnjudgeableRenewal(t *testing.T) {
	ctx := context.Background()
	svc, st, write := newResolveFixtureDB(t)

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)

	sec := mustOAuthSecret(t, st, "claude", model.SecretValue{
		Token:                "refused-access",
		RefreshToken:         "good-refresh",
		TokenURL:             down.URL,
		ClientID:             "client-1",
		AccessTokenExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	})
	mustGrant(t, st, sec.ID, model.SecretGrantScopeProject, "project-1")
	createSandbox(t, st, "sb-1", "pool-1")
	mustAssign(t, st, "sb-1", sec.ID, "SENTINEL-A")

	// A rejection already stands, judged while the endpoint still answered.
	if err := st.RecordSecretRejection(ctx, &model.SecretRejection{
		ProjectID: "project-1", SecretID: sec.ID, Host: "api.anthropic.com",
		Reason: model.SecretRejectionReasonRefreshFailed, SandboxID: "sb-1",
	}); err != nil {
		t.Fatalf("record rejection: %v", err)
	}
	// Past the judge cooldown, so the next report re-judges rather than taking
	// the standing-row branch.
	if err := write.WithContext(ctx).Model(&model.SecretRejection{}).
		Where("secret_id = ?", sec.ID).
		Update("last_seen_at", time.Now().Add(-20*time.Minute)).Error; err != nil {
		t.Fatalf("age the rejection: %v", err)
	}

	if err := svc.RecordSandboxSecretRejection(ctx, "pool-1", "sb-1", "SENTINEL-A", "api.anthropic.com", resourcesecrets.SecretRejectedOutcome, ""); err != nil {
		t.Fatalf("record rejection: %v", err)
	}

	got := mustRejection(t, st, sec.ID, "api.anthropic.com")
	if got.Reason != model.SecretRejectionReasonRefreshFailed {
		t.Fatalf("reason = %q, want the standing reason kept rather than re-invented", got.Reason)
	}
	if time.Since(got.LastSeenAt) > time.Minute {
		t.Fatalf("lastSeenAt = %v, want the sighting recorded so the row does not age out mid-incident", got.LastSeenAt)
	}
	// And it is still listed, which is the half the user sees.
	listed, err := svc.ListSecretRejections(ctx, "project-1")
	if err != nil {
		t.Fatalf("list rejections: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("rejections = %v, want the credential still reported as refused", listed)
	}
}
