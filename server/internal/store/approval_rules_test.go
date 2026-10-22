package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/policy"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func rule(projectID string) *model.ApprovalRule {
	return &model.ApprovalRule{
		ProjectID: projectID, WellKnownID: "com.github.api", Hosts: []string{"github.com"},
		Purpose: model.SecretGrantPurposeUse, MaxGrantTTL: 3600, Enabled: true,
		Policy: &policy.Policy{Statements: []policy.Statement{{
			Effect: policy.Allow, Actions: []string{policy.ActionRead}, ResourceType: "issue",
			Scope: policy.Scope{Type: "repository", Name: policy.VarPrimaryRepo},
		}}, Definitions: map[string]string{"issue": "a GitHub issue", "repository": "a GitHub repository"}},
	}
}

// A rule is a project's row: it goes with the project, which could not
// otherwise be deleted for the rows still pointing at it, and nobody else's
// goes with it.
func TestDeleteProjectRemovesItsApprovalRulesAndNoOneElses(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	doomed := &model.Project{Name: "doomed"}
	kept := &model.Project{Name: "kept"}
	for _, project := range []*model.Project{doomed, kept} {
		if err := st.CreateProject(ctx, project); err != nil {
			t.Fatalf("create project: %v", err)
		}
		if err := st.CreateApprovalRule(ctx, rule(project.ID)); err != nil {
			t.Fatalf("create rule: %v", err)
		}
	}
	if err := st.DeleteProject(ctx, doomed.ID); err != nil {
		t.Fatalf("DeleteProject() with an approval rule: %v", err)
	}
	if rows, err := st.ListApprovalRules(ctx, doomed.ID); err != nil || len(rows) != 0 {
		t.Fatalf("deleted project's rules = %d, %v; want none", len(rows), err)
	}
	if rows, err := st.ListApprovalRules(ctx, kept.ID); err != nil || len(rows) != 1 {
		t.Fatalf("other project's rules = %d, %v; want its one rule untouched", len(rows), err)
	}
}

// An edit stores what it was given, zeros included: a rule switched off and a
// cap set to zero are both the zero value, and a default would put back the
// rule that was on and the cap it had.
func TestAnApprovalRuleKeepsItsZeros(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	project := &model.Project{Name: "project"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	off := rule(project.ID)
	off.Enabled = false
	if err := st.CreateApprovalRule(ctx, off); err != nil {
		t.Fatal(err)
	}
	if got, err := st.GetApprovalRule(ctx, project.ID, off.ID); err != nil || got.Enabled {
		t.Fatalf("a rule created switched off = %+v, %v", got, err)
	}
	if enabled, err := st.HasEnabledApprovalRule(ctx, project.ID); err != nil || enabled {
		t.Fatalf("HasEnabledApprovalRule() = %v, %v, want false with only a disabled rule", enabled, err)
	}

	on := rule(project.ID)
	if err := st.CreateApprovalRule(ctx, on); err != nil {
		t.Fatal(err)
	}
	created := on.CreatedAt
	on.Enabled, on.MaxGrantTTL, on.Repositories = false, 0, nil
	if err := st.UpdateApprovalRule(ctx, on); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetApprovalRule(ctx, project.ID, on.ID)
	if err != nil || got.Enabled || got.MaxGrantTTL != 0 || !got.CreatedAt.Equal(created) {
		t.Fatalf("edited rule = %+v, %v, want it off, uncapped, and created when it was", got, err)
	}
	if err := st.UpdateApprovalRule(ctx, rule("another-project")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("editing a rule that is not there = %v, want not found", err)
	}
	if err := st.DeleteApprovalRule(ctx, project.ID, on.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteApprovalRule(ctx, project.ID, on.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleting a rule twice = %v, want not found", err)
	}
}

// An answer on record is found by the question it answered: the discobox, the
// request and the digest. The newest answer is the one that stands.
func TestAnAnsweredQuestionIsFoundByItsDigest(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	project := &model.Project{Name: "project"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	record := func(kind, sandboxID, requestID, digest, ruleID string, allow bool, at time.Time) {
		t.Helper()
		if err := st.CreateCredentialVerdict(ctx, &model.CredentialVerdict{
			ProjectID: project.ID, Kind: kind, Origin: model.CredentialVerdictOriginJudge, SandboxID: sandboxID,
			SecretRequestID: requestID, QuestionDigest: digest, RuleID: ruleID, Allow: allow, Reason: "decided", CreatedAt: at,
		}); err != nil {
			t.Fatalf("record verdict: %v", err)
		}
	}
	now := time.Now().UTC()
	record(model.CredentialVerdictKindRule, "sbx_1", "sreq_1", "digest-rule", "", false, now.Add(-time.Minute))
	record(model.CredentialVerdictKindRule, "sbx_1", "sreq_1", "digest-rule", "rule_1", true, now)
	record(model.CredentialVerdictKindDelegation, "sbx_1", "sreq_3", "digest-delegation", "", true, now)
	// A discobox's own judge's word is never an answer to a rule's question.
	if err := st.CreateCredentialVerdict(ctx, &model.CredentialVerdict{
		ProjectID: project.ID, Kind: model.CredentialVerdictKindRule, Origin: model.CredentialVerdictOriginSandbox,
		SandboxID: "sbx_1", SecretRequestID: "sreq_9", QuestionDigest: "digest-volunteered", Allow: true,
	}); err != nil {
		t.Fatal(err)
	}

	for _, fresh := range []bool{true, false} {
		if got, err := st.AnsweredQuestion(ctx, project.ID, "sbx_1", "sreq_1", "digest-rule", fresh); err != nil || !got.Allow {
			t.Fatalf("rule answer = %+v, %v, want the newest", got, err)
		}
	}
	for name, miss := range map[string][3]string{
		"another request":    {"sbx_1", "sreq_2", "digest-rule"},
		"another discobox":   {"sbx_2", "sreq_1", "digest-rule"},
		"another kind":       {"sbx_1", "sreq_3", "digest-delegation"},
		"an edited question": {"sbx_1", "sreq_1", "digest-edited"},
		"a volunteered row":  {"sbx_1", "sreq_9", "digest-volunteered"},
	} {
		if _, err := st.AnsweredQuestion(ctx, project.ID, miss[0], miss[1], miss[2], true); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s = %v, want no answer on record", name, err)
		}
	}
	for ruleID, want := range map[string]bool{"rule_1": true, "rule_2": false} {
		if named, err := st.QuestionRecordedUnder(ctx, project.ID, "sbx_1", "sreq_1", "digest-rule", ruleID); err != nil || named != want {
			t.Errorf("QuestionRecordedUnder(%s) = %v, %v, want %v", ruleID, named, err, want)
		}
	}
}
