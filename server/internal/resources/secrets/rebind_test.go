package secrets_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// rebindBody binds secret in GH_TOKEN on the asking discobox, replacing
// replaces when it is not empty.
func rebindBody(secret, replaces string) services.CreateSecretBindingBody {
	body := services.CreateSecretBindingBody{
		Secret: secret, TargetKind: serverapi.CreateSecretBindingBodyTargetKindSandbox, Target: serverapi.NewOptString(testSandboxID),
		EnvVar: serverapi.NewOptString("GH_TOKEN"),
	}
	if replaces != "" {
		body.Replaces = serverapi.NewOptString(replaces)
	}
	return body
}

// approveReplacing approves the use part of a request with secretID, naming
// the binding it replaces when replaces is not empty.
func approveReplacing(secretID, replaces string) services.ApproveSecretRequestBody {
	decision := serverapi.SecretRequestPartDecision{Decision: serverapi.SecretRequestPartDecisionDecisionApprove}
	if replaces != "" {
		decision.Replaces = serverapi.NewOptString(replaces)
	}
	return services.ApproveSecretRequestBody{SecretId: serverapi.NewOptString(secretID), Use: serverapi.NewOptSecretRequestPartDecision(decision)}
}

// askUse files a request to use a credential in GH_TOKEN.
func askUse(ctx context.Context, t *testing.T, svc *resourcesecrets.Service) *model.SecretRequest {
	t.Helper()
	req, err := svc.CreateSandboxCredentialRequest(ctx, testPoolID, services.CreateSandboxCredentialRequestBody{
		SandboxId: testSandboxID, Name: "github", EnvVar: serverapi.NewOptString("GH_TOKEN"), Hosts: []string{"github.com"},
	})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	return req
}

// twoSecrets are the secret the discobox holds GH_TOKEN with now, and the
// one a person rebinds it to.
func twoSecrets(ctx context.Context, t *testing.T, svc *resourcesecrets.Service) (personal, team *model.Secret) {
	t.Helper()
	return createRoutedSecret(ctx, t, svc, "github", "github.com", 0), createRoutedSecret(ctx, t, svc, "github-team", "github.com", 0)
}

// useOfGHToken is the discobox's own plain use binding in GH_TOKEN.
func useOfGHToken(t *testing.T, st *store.Store) *model.SecretBinding {
	t.Helper()
	bindings, err := st.ListSecretBindings(context.Background(), "project-1", store.SecretBindingFilter{TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID})
	if err != nil {
		t.Fatal(err)
	}
	var found *model.SecretBinding
	for i := range bindings {
		if bindings[i].Use && !bindings[i].Adds && bindings[i].EnvName == "GH_TOKEN" {
			if found != nil {
				t.Fatalf("two plain bindings in GH_TOKEN: %+v", bindings)
			}
			found = &bindings[i]
		}
	}
	return found
}

// historyOf is a binding's events, oldest first.
func historyOf(t *testing.T, svc *resourcesecrets.Service, bindingID string) []model.SecretBindingEvent {
	t.Helper()
	events, err := svc.ListSecretBindingEvents(testPrincipalContext(), "project-1", services.SecretBindingEventQuery{BindingID: bindingID})
	if err != nil {
		t.Fatal(err)
	}
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	return events
}

func eventKinds(events []model.SecretBindingEvent) []string {
	var out []string
	for _, event := range events {
		out = append(out, event.Event)
	}
	return out
}

// A person rebinds a variable directly by naming the binding it replaces:
// a use-only binding goes, the new secret's binding takes the variable in the
// same write, and one rebound event, in both bindings' histories, names both
// secrets and who did it (ADR 26-11-19-927 §§2, 4, 8). The old secret's
// addition in the variable goes with it; its binding in another variable
// stays.
func TestAPersonRebindsAVariableDirectly(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	personal, team := twoSecrets(ctx, t, svc)
	old := mustDeliverAs(t, st, testSandboxID, personal.ID, "GH_TOKEN")
	added := mustBindAs(t, st, &model.SecretBinding{SecretID: personal.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, Adds: true, EnvName: "GH_TOKEN"})
	elsewhere := mustDeliverAs(t, st, testSandboxID, personal.ID, "GH_OTHER")
	marks := &sentinelMarks{}
	svc.SetSecretHolders(marks)

	made, err := svc.CreateSecretBinding(ctx, "project-1", rebindBody("github-team", old.ID))
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if made.SecretID != team.ID || made.EnvName != "GH_TOKEN" || made.ParentID != "" {
		t.Fatalf("made = %+v, want the team secret in GH_TOKEN, the person's", made)
	}
	ids := bindingIDs(t, st)
	if ids[old.ID] || ids[added.ID] || !ids[elsewhere.ID] || !ids[made.ID] {
		t.Fatalf("bindings = %v, want %s and its addition %s gone, %s in GH_OTHER kept", ids, old.ID, added.ID, elsewhere.ID)
	}
	marked := map[string]bool{}
	for _, b := range marks.bindings {
		marked[b.ID] = true
	}
	if !marked[old.ID] || !marked[made.ID] || !marked[added.ID] {
		t.Fatalf("marked %v, want the replaced binding, its addition and the new one", marked)
	}

	history := historyOf(t, svc, made.ID)
	if kinds := eventKinds(history); len(kinds) != 1 || kinds[0] != model.SecretBindingEventRebound {
		t.Fatalf("new binding's history = %v, want one rebound and no created", kinds)
	}
	rebound := history[0]
	if rebound.Replaced == nil || rebound.Replaced.ID != old.ID || rebound.Replaced.SecretID != personal.ID || rebound.Replaced.SecretName != "github" ||
		rebound.Binding.SecretID != team.ID || rebound.Actor != "user-1" {
		t.Fatalf("rebound = %+v, want both bindings, both secrets and the person", rebound)
	}
	if kinds := eventKinds(historyOf(t, svc, old.ID)); len(kinds) != 2 || kinds[0] != model.SecretBindingEventCreated || kinds[1] != model.SecretBindingEventRebound {
		t.Fatalf("replaced binding's history = %v, want created then rebound, and no deleted", kinds)
	}
	bySecret, err := svc.ListSecretBindingEvents(ctx, "project-1", services.SecretBindingEventQuery{Secret: "github"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range bySecret {
		found = found || event.ID == rebound.ID
	}
	if !found {
		t.Fatalf("the old secret's history misses the rebound")
	}
}

// A binding that also delegates is split, never deleted: it keeps its ID,
// its delegation and what was handed on under it, and only its use goes
// (ADR 26-11-19-927 §4), on either path.
func TestARebindKeepsTheReplacedBindingsDelegation(t *testing.T) {
	for _, path := range []string{"direct", "approval"} {
		t.Run(path, func(t *testing.T) {
			ctx := testPrincipalContext()
			svc, st := newAgentCredentialService(t)
			personal, team := twoSecrets(ctx, t, svc)
			combined := mustBindAs(t, st, &model.SecretBinding{SecretID: personal.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, Delegate: true, EnvName: "GH_TOKEN"})
			child := handOnTo(t, st, personal.ID, combined.ID)

			var madeID string
			if path == "direct" {
				made, err := svc.CreateSecretBinding(ctx, "project-1", rebindBody("github-team", combined.ID))
				if err != nil {
					t.Fatalf("rebind: %v", err)
				}
				madeID = made.ID
			} else {
				req := askUse(ctx, t, svc)
				answered, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, approveReplacing(team.ID, combined.ID))
				if err != nil {
					t.Fatalf("approve: %v", err)
				}
				madeID = answered.Use.BindingID
				if history := historyOf(t, svc, madeID); len(history) != 1 || history[0].Cause != req.ID {
					t.Fatalf("history = %+v, want one rebound caused by %s", history, req.ID)
				}
			}
			split, err := st.SecretBindingByExactID(ctx, "project-1", combined.ID)
			if err != nil || split.Use || !split.Delegate || split.EnvName != "" {
				t.Fatalf("split = %+v, %v; want the delegation alone, kept", split, err)
			}
			if _, err := st.SecretBindingByExactID(ctx, "project-1", child.ID); err != nil {
				t.Fatalf("child handed on under it = %v, want kept", err)
			}
			if use := useOfGHToken(t, st); use == nil || use.ID != madeID || use.SecretID != team.ID {
				t.Fatalf("GH_TOKEN = %+v, want the team secret's binding %s", use, madeID)
			}
		})
	}
}

// What each path may take over (ADR 26-11-19-927 §3): a person directly may
// rebind a binding handed on under a delegation or one handing over the value
// itself; an approval may not, and says why, writing nothing.
func TestWhatEachPathMayRebind(t *testing.T) {
	for name, tc := range map[string]struct {
		standing func(st *store.Store, secretID string) *model.SecretBinding
		approval string // the refusal's words, on the approval path
	}{
		"handed on under a delegation": {
			standing: func(st *store.Store, secretID string) *model.SecretBinding {
				parent := mustBindAs(t, st, &model.SecretBinding{SecretID: secretID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true})
				return mustBindAs(t, st, &model.SecretBinding{SecretID: secretID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "GH_TOKEN", ParentID: parent.ID})
			},
			approval: "under a delegation",
		},
		"raw": {
			standing: func(st *store.Store, secretID string) *model.SecretBinding {
				return mustBindAs(t, st, &model.SecretBinding{SecretID: secretID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, EnvName: "GH_TOKEN", Raw: "env"})
			},
			approval: "value itself",
		},
		"combined and raw": {
			standing: func(st *store.Store, secretID string) *model.SecretBinding {
				return mustBindAs(t, st, &model.SecretBinding{SecretID: secretID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID, Use: true, Delegate: true, EnvName: "GH_TOKEN", Raw: "env"})
			},
			approval: "value itself",
		},
	} {
		t.Run(name+"/approval", func(t *testing.T) {
			ctx := testPrincipalContext()
			svc, st := newAgentCredentialService(t)
			personal, team := twoSecrets(ctx, t, svc)
			allowRaw(ctx, t, svc, personal)
			standing := tc.standing(st, personal.ID)
			before := bindingIDs(t, st)
			req := askUse(ctx, t, svc)
			_, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, approveReplacing(team.ID, standing.ID))
			if !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), standing.ID) || !strings.Contains(err.Error(), tc.approval) || !strings.Contains(err.Error(), "directly") {
				t.Fatalf("approve = %v, want a 409 naming %s, saying %q and to rebind it directly", err, standing.ID, tc.approval)
			}
			if after := bindingIDs(t, st); len(after) != len(before) {
				t.Fatalf("bindings = %v, want %v untouched", after, before)
			}
			if got, _ := st.GetSecretRequest(ctx, "project-1", req.ID); got.Status != model.SecretRequestStatusPending {
				t.Fatalf("request = %+v, want it still pending", got)
			}
		})
		t.Run(name+"/direct", func(t *testing.T) {
			ctx := testPrincipalContext()
			svc, st := newAgentCredentialService(t)
			personal, team := twoSecrets(ctx, t, svc)
			allowRaw(ctx, t, svc, personal)
			standing := tc.standing(st, personal.ID)
			made, err := svc.CreateSecretBinding(ctx, "project-1", rebindBody("github-team", standing.ID))
			if err != nil {
				t.Fatalf("rebind: %v", err)
			}
			if made.ParentID != "" || made.Raw != "" || made.SecretID != team.ID {
				t.Fatalf("made = %+v, want the person's sentinel binding of the team secret", made)
			}
			if use := useOfGHToken(t, st); use == nil || use.ID != made.ID {
				t.Fatalf("GH_TOKEN = %+v, want %s", use, made.ID)
			}
		})
	}
}

// A rebind names the binding it replaces, which must be the one delivering
// the variable when it commits; anything else is refused naming the binding
// that delivers it now (ADR 26-11-19-927 §2).
func TestARebindNamesTheBindingItReplaces(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	personal, team := twoSecrets(ctx, t, svc)
	old := mustDeliverAs(t, st, testSandboxID, personal.ID, "GH_TOKEN")
	ownTeam := mustDeliverAs(t, st, testSandboxID, team.ID, "GH_TEAM")
	other := mustDeliverAs(t, st, testSandboxID, personal.ID, "GH_OTHER")

	for name, tc := range map[string]struct {
		replaces string
		status   int
		says     []string
	}{
		"none named":                   {"", http.StatusConflict, []string{old.ID, "github"}},
		"a binding no longer there":    {"sbd_gone", http.StatusConflict, []string{"sbd_gone", old.ID}},
		"another variable's binding":   {other.ID, http.StatusConflict, []string{other.ID, old.ID, "no longer delivers"}},
		"a binding of the same secret": {ownTeam.ID, http.StatusConflict, []string{ownTeam.ID, "nothing to rebind"}},
	} {
		t.Run("direct/"+name, func(t *testing.T) {
			_, err := svc.CreateSecretBinding(ctx, "project-1", rebindBody("github-team", tc.replaces))
			if !isStatus(err, tc.status) {
				t.Fatalf("create = %v, want %d", err, tc.status)
			}
			for _, word := range tc.says {
				if !strings.Contains(err.Error(), word) {
					t.Fatalf("create = %v, want it to say %q", err, word)
				}
			}
		})
	}
	req := askUse(ctx, t, svc)
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, approveReplacing(team.ID, "")); !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), old.ID) {
		t.Fatalf("approve with no binding named = %v, want a 409 naming %s", err, old.ID)
	}
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, approveReplacing(team.ID, "sbd_gone")); !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), old.ID) {
		t.Fatalf("approve naming a stale binding = %v, want a 409 naming %s", err, old.ID)
	}
	if use := useOfGHToken(t, st); use == nil || use.ID != old.ID {
		t.Fatalf("GH_TOKEN = %+v, want %s still", use, old.ID)
	}

	// With nothing of another secret there, naming one is refused too.
	if _, err := svc.CreateSecretBinding(ctx, "project-1", services.CreateSecretBindingBody{
		Secret: "github-team", TargetKind: serverapi.CreateSecretBindingBodyTargetKindSandbox, Target: serverapi.NewOptString(testSandboxID),
		EnvVar: serverapi.NewOptString("GH_EMPTY"), Replaces: serverapi.NewOptString(old.ID),
	}); !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "no longer delivers") {
		t.Fatalf("rebind of an empty variable = %v, want 409", err)
	}
	// A rebind is of one discobox's variable.
	if _, err := svc.CreateSecretBinding(ctx, "project-1", services.CreateSecretBindingBody{
		Secret: "github-team", TargetKind: serverapi.CreateSecretBindingBodyTargetKindProject,
		EnvVar: serverapi.NewOptString("GH_TOKEN"), Replaces: serverapi.NewOptString(old.ID),
	}); !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("rebind on a project = %v, want 400", err)
	}
}

// Only a person rebinds: a lead answering its child's request may not name
// a binding to replace, and without one is refused as before; replaces is a
// use part's, granted as a replacement (ADR 26-11-19-927 §§1-2).
func TestOnlyAPersonsReplacementRebinds(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	personal, team := twoSecrets(ctx, t, svc)
	old := mustDeliverAs(t, st, testSandboxID, personal.ID, "GH_TOKEN")
	mustBindAs(t, st, &model.SecretBinding{SecretID: team.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true})
	req := askUse(ctx, t, svc)

	if _, err := svc.ApproveSecretRequest(asLead(), "project-1", req.ID, approveReplacing(team.ID, old.ID)); !isStatus(err, http.StatusForbidden) {
		t.Fatalf("a lead naming a binding to replace = %v, want 403", err)
	}
	if _, err := svc.ApproveSecretRequest(asLead(), "project-1", req.ID, approveReplacing(team.ID, "")); !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "only a person") {
		t.Fatalf("a lead answering over another secret = %v, want 409 saying only a person rebinds", err)
	}
	add := approveReplacing(team.ID, old.ID)
	decision := add.Use.Value
	decision.As = serverapi.NewOptSecretRequestPartDecisionAs(serverapi.SecretRequestPartDecisionAsAdd)
	add.Use = serverapi.NewOptSecretRequestPartDecision(decision)
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, add); !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("an addition naming a binding to replace = %v, want 400", err)
	}
	deny := services.ApproveSecretRequestBody{Use: serverapi.NewOptSecretRequestPartDecision(serverapi.SecretRequestPartDecision{
		Decision: serverapi.SecretRequestPartDecisionDecisionDeny, Replaces: serverapi.NewOptString(old.ID),
	})}
	if _, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, deny); !isStatus(err, http.StatusBadRequest) {
		t.Fatalf("a denial naming a binding to replace = %v, want 400", err)
	}
	if use := useOfGHToken(t, st); use == nil || use.ID != old.ID {
		t.Fatalf("GH_TOKEN = %+v, want %s still", use, old.ID)
	}
}

// A person's approval naming the binding rebinds the variable, and the new
// secret's own use bindings are replaced as any approval replaces them (ADR
// 26-11-02-022 §5): its plain binding in another variable is the one kept,
// moved into this one.
func TestAnApprovalRebindsAndReplacesTheNewSecretsUse(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	personal, team := twoSecrets(ctx, t, svc)
	old := mustDeliverAs(t, st, testSandboxID, personal.ID, "GH_TOKEN")
	ownTeam := mustDeliverAs(t, st, testSandboxID, team.ID, "GH_TEAM")
	req := askUse(ctx, t, svc)

	answered, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, approveReplacing(team.ID, old.ID))
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if answered.Use.BindingID != ownTeam.ID {
		t.Fatalf("binding = %s, want the team secret's own binding %s kept", answered.Use.BindingID, ownTeam.ID)
	}
	if use := useOfGHToken(t, st); use == nil || use.ID != ownTeam.ID {
		t.Fatalf("GH_TOKEN = %+v, want %s", use, ownTeam.ID)
	}
	if ids := bindingIDs(t, st); ids[old.ID] {
		t.Fatalf("bindings = %v, want %s gone", ids, old.ID)
	}
	history := historyOf(t, svc, ownTeam.ID)
	if last := history[len(history)-1]; last.Event != model.SecretBindingEventRebound || last.Replaced == nil || last.Replaced.ID != old.ID {
		t.Fatalf("history = %v, want it to end on a rebound of %s", eventKinds(history), old.ID)
	}
}

// An approval checks every binding a rebind would touch, not only the one it
// names: an addition of the old secret a lead handed on under its delegation
// is not taken over by an approval, which writes nothing; a person directly
// may (ADR 26-11-19-927 §3, 26-11-02-022 §5).
func TestARebindByApprovalTakesOverNoHandedOnAddition(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	personal, team := twoSecrets(ctx, t, svc)
	old := mustDeliverAs(t, st, testSandboxID, personal.ID, "GH_TOKEN")
	parent := mustBindAs(t, st, &model.SecretBinding{SecretID: personal.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: leadID, Delegate: true})
	handedOn := mustBindAs(t, st, &model.SecretBinding{SecretID: personal.ID, TargetKind: model.SecretBindingTargetSandbox, TargetID: testSandboxID,
		Use: true, Adds: true, EnvName: "GH_TOKEN", ParentID: parent.ID})
	before := bindingIDs(t, st)
	req := askUse(ctx, t, svc)

	_, err := svc.ApproveSecretRequest(ctx, "project-1", req.ID, approveReplacing(team.ID, old.ID))
	if !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), handedOn.ID) || !strings.Contains(err.Error(), "delegation") {
		t.Fatalf("approve = %v, want a 409 naming the handed-on addition %s", err, handedOn.ID)
	}
	if after := bindingIDs(t, st); len(after) != len(before) {
		t.Fatalf("bindings = %v, want %v untouched", after, before)
	}
	if _, err := svc.CreateSecretBinding(ctx, "project-1", rebindBody("github-team", old.ID)); err != nil {
		t.Fatalf("direct rebind: %v", err)
	}
	if ids := bindingIDs(t, st); ids[handedOn.ID] || ids[old.ID] {
		t.Fatalf("bindings = %v, want the old secret's binding and addition in GH_TOKEN gone", ids)
	}
}
