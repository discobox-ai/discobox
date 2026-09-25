package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// verdictFixture records a spread of verdicts across two projects and returns
// the store and the project the assertions read.
func verdictFixture(t *testing.T) (*store.Store, string) {
	t.Helper()
	ctx := context.Background()
	st := newTestStore(t)
	project := &model.Project{Name: "verdicts"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	other := &model.Project{Name: "elsewhere"}
	if err := st.CreateProject(ctx, other); err != nil {
		t.Fatalf("create other project: %v", err)
	}
	for _, v := range []model.CredentialVerdict{
		// None of these sandboxes exist. The trail outlives them, so the read
		// must not need them to.
		{ID: "cv_1", ProjectID: project.ID, SandboxID: "sbx_a", GrantID: "grant_1", UseID: "use_1", Allow: true, Role: "judge", Prompt: "p", CreatedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)},
		{ID: "cv_2", ProjectID: project.ID, SandboxID: "sbx_a", GrantID: "grant_1", UseID: "use_1", Allow: false, Volunteered: true, Role: "judge", Prompt: "p", CreatedAt: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)},
		{ID: "cv_3", ProjectID: project.ID, Kind: model.CredentialVerdictKindRequest, Origin: model.CredentialVerdictOriginJudge, SandboxID: "sbx_b", GrantID: "grant_2", UseID: "use_2", Allow: true, Role: "judge", Prompt: "p", CreatedAt: time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)},
		{ID: "cv_4", ProjectID: other.ID, SandboxID: "sbx_a", GrantID: "grant_1", UseID: "use_1", Allow: true, Role: "judge", Prompt: "p", CreatedAt: time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)},
	} {
		if err := st.CreateCredentialVerdict(ctx, &v); err != nil {
			t.Fatalf("create verdict %s: %v", v.ID, err)
		}
	}
	return st, project.ID
}

func verdictIDs(rows []model.CredentialVerdict) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func TestListCredentialVerdictsFilters(t *testing.T) {
	st, projectID := verdictFixture(t)
	allowed, denied := true, false
	for _, tc := range []struct {
		name   string
		filter store.CredentialVerdictFilter
		want   []string
	}{
		// Newest first, and nothing from the other project even where every
		// other field would match.
		{name: "whole project", filter: store.CredentialVerdictFilter{}, want: []string{"cv_3", "cv_2", "cv_1"}},
		{name: "sandbox", filter: store.CredentialVerdictFilter{SandboxID: "sbx_a"}, want: []string{"cv_2", "cv_1"}},
		{name: "use", filter: store.CredentialVerdictFilter{UseID: "use_2"}, want: []string{"cv_3"}},
		{name: "grant", filter: store.CredentialVerdictFilter{GrantID: "grant_1"}, want: []string{"cv_2", "cv_1"}},
		{name: "allowed only", filter: store.CredentialVerdictFilter{Allow: &allowed}, want: []string{"cv_3", "cv_1"}},
		{name: "denied only", filter: store.CredentialVerdictFilter{Allow: &denied}, want: []string{"cv_2"}},
		{name: "since is inclusive", filter: store.CredentialVerdictFilter{Since: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}, want: []string{"cv_3", "cv_2"}},
		{name: "until is inclusive", filter: store.CredentialVerdictFilter{Until: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}, want: []string{"cv_2", "cv_1"}},
		{name: "limit keeps the newest", filter: store.CredentialVerdictFilter{Limit: 2}, want: []string{"cv_3", "cv_2"}},
		{name: "forward keeps the oldest", filter: store.CredentialVerdictFilter{Ascending: true, Limit: 2}, want: []string{"cv_1", "cv_2"}},
		{name: "forward from a cursor", filter: store.CredentialVerdictFilter{Ascending: true, Since: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)}, want: []string{"cv_2", "cv_3"}},
		{name: "requests", filter: store.CredentialVerdictFilter{Kind: model.CredentialVerdictKindRequest}, want: []string{"cv_3"}},
		{name: "commands", filter: store.CredentialVerdictFilter{Kind: model.CredentialVerdictKindCommand}, want: []string{"cv_2", "cv_1"}},
		{name: "filters compose", filter: store.CredentialVerdictFilter{SandboxID: "sbx_a", Allow: &allowed}, want: []string{"cv_1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := st.ListCredentialVerdicts(context.Background(), projectID, tc.filter)
			if err != nil {
				t.Fatalf("ListCredentialVerdicts() error = %v", err)
			}
			got := verdictIDs(rows)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// A denial reported by the sandbox must come back marked as such, so a reader
// can tell a record of an issue from a report nothing forced.
func TestListCredentialVerdictsKeepsProvenance(t *testing.T) {
	st, projectID := verdictFixture(t)
	rows, err := st.ListCredentialVerdicts(context.Background(), projectID, store.CredentialVerdictFilter{UseID: "use_1"})
	if err != nil {
		t.Fatalf("ListCredentialVerdicts() error = %v", err)
	}
	if len(rows) != 2 || rows[0].ID != "cv_2" || !rows[0].Volunteered || rows[1].Volunteered {
		t.Fatalf("provenance lost: %+v", rows)
	}
}

// A verdict written without a kind is a command verdict from the sandbox, which
// is what every row recorded before request verdicts existed was. The columns'
// defaults are the whole upgrade, so they are what this holds to.
func TestAVerdictWithNoKindIsTheSandboxsCommandVerdict(t *testing.T) {
	st, projectID := verdictFixture(t)
	rows, err := st.ListCredentialVerdicts(context.Background(), projectID, store.CredentialVerdictFilter{ID: "cv_1"})
	if err != nil {
		t.Fatalf("ListCredentialVerdicts() error = %v", err)
	}
	if len(rows) != 1 || rows[0].Kind != model.CredentialVerdictKindCommand || rows[0].Origin != model.CredentialVerdictOriginSandbox {
		t.Fatalf("rows = %+v, want cv_1 read back as the sandbox's command verdict", rows)
	}
}

// A project's verdicts go with the project. They reference it, so before they
// were in the cascade a project that had recorded even one verdict failed to
// delete on the foreign key — and ADR 0023 only deletes a project once it is
// empty, so no such project could ever be deleted.
func TestDeleteProjectRemovesItsVerdictsAndNoOneElses(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	doomed := &model.Project{Name: "doomed"}
	kept := &model.Project{Name: "kept"}
	for _, project := range []*model.Project{doomed, kept} {
		if err := st.CreateProject(ctx, project); err != nil {
			t.Fatalf("create project: %v", err)
		}
		if err := st.CreateCredentialVerdict(ctx, &model.CredentialVerdict{
			ProjectID: project.ID, SandboxID: "sbx_gone", UseID: "use_1", Role: "judge", Prompt: "p",
		}); err != nil {
			t.Fatalf("create verdict: %v", err)
		}
	}

	if err := st.DeleteProject(ctx, doomed.ID); err != nil {
		t.Fatalf("DeleteProject() with recorded verdicts: %v", err)
	}
	if rows, err := st.ListCredentialVerdicts(ctx, doomed.ID, store.CredentialVerdictFilter{}); err != nil || len(rows) != 0 {
		t.Fatalf("deleted project's verdicts = %d, %v; want none", len(rows), err)
	}
	if rows, err := st.ListCredentialVerdicts(ctx, kept.ID, store.CredentialVerdictFilter{}); err != nil || len(rows) != 1 {
		t.Fatalf("other project's verdicts = %d, %v; want its one verdict untouched", len(rows), err)
	}
}

// On SQLite a time is text carrying its offset, compared as text. A server
// outside UTC used to stamp created_at in its own zone while a caller bound
// `since` in another, and the two disagreed by the difference: nine hours east
// of UTC, "since an hour from now" still matched a verdict written a moment
// ago. The server's zone is swapped here rather than read from the environment,
// so this fails the same way wherever it runs.
func TestListCredentialVerdictsSinceDoesNotDependOnZones(t *testing.T) {
	serverZone := time.Local
	time.Local = time.FixedZone("UTC+9", 9*60*60)
	t.Cleanup(func() { time.Local = serverZone })

	ctx := context.Background()
	st := newTestStore(t)
	project := &model.Project{Name: "zones"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := st.CreateCredentialVerdict(ctx, &model.CredentialVerdict{
		ProjectID: project.ID, SandboxID: "sbx_a", UseID: "use_1", Role: "judge", Prompt: "p",
	}); err != nil {
		t.Fatalf("create verdict: %v", err)
	}

	// The caller is somewhere else again.
	callerZone := time.FixedZone("UTC-7", -7*60*60)
	now := time.Now().In(callerZone)
	for _, tc := range []struct {
		name  string
		since time.Time
		want  int
	}{
		{name: "an hour ago", since: now.Add(-time.Hour), want: 1},
		{name: "an hour from now", since: now.Add(time.Hour), want: 0},
	} {
		rows, err := st.ListCredentialVerdicts(ctx, project.ID, store.CredentialVerdictFilter{Since: tc.since})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(rows) != tc.want {
			t.Fatalf("since %s: got %d verdicts, want %d", tc.name, len(rows), tc.want)
		}
	}
}

// A standing allow is found for its own discobox and use while it stands, and
// never once it has lapsed, for another discobox, or on a row that is not the
// project judge's allow.
func TestStandingVerdictsAreTheLiveAllowsForOneUse(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	project := &model.Project{Name: "standing"}
	if err := st.CreateProject(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time {
		// A zone other than UTC, which the row must not be compared in.
		t := now.Add(d).In(time.FixedZone("JST", 9*60*60))
		return &t
	}
	request := func(id, sandboxID, useID string, allow bool, route string, until *time.Time, created time.Duration) model.CredentialVerdict {
		return model.CredentialVerdict{
			ID: id, ProjectID: project.ID, Kind: model.CredentialVerdictKindRequest, Origin: model.CredentialVerdictOriginJudge,
			SandboxID: sandboxID, UseID: useID, Allow: allow, StandingRoute: route, StandingUntil: until,
			CreatedAt: now.Add(created),
		}
	}
	for _, v := range []model.CredentialVerdict{
		request("cv_live_old", "sbx_a", "use_1", true, "GET /a", at(5*time.Minute), -2*time.Minute),
		request("cv_live_new", "sbx_a", "use_1", true, "GET /b", at(10*time.Minute), -time.Minute),
		request("cv_lapsed", "sbx_a", "use_1", true, "GET /c", at(-time.Second), -20*time.Minute),
		request("cv_no_route", "sbx_a", "use_1", true, "", nil, -time.Minute),
		request("cv_refused", "sbx_a", "use_1", false, "GET /d", at(5*time.Minute), -time.Minute),
		request("cv_other_sandbox", "sbx_b", "use_1", true, "GET /e", at(5*time.Minute), -time.Minute),
		request("cv_other_use", "sbx_a", "use_2", true, "GET /f", at(5*time.Minute), -time.Minute),
		{ID: "cv_command", ProjectID: project.ID, SandboxID: "sbx_a", UseID: "use_1", Allow: true, StandingRoute: "GET /g", StandingUntil: at(5 * time.Minute), CreatedAt: now},
	} {
		if err := st.CreateCredentialVerdict(ctx, &v); err != nil {
			t.Fatalf("create verdict %s: %v", v.ID, err)
		}
	}
	rows, err := st.StandingVerdicts(ctx, project.ID, "sbx_a", "use_1", now)
	if err != nil {
		t.Fatalf("StandingVerdicts() error = %v", err)
	}
	if got, want := verdictIDs(rows), []string{"cv_live_new", "cv_live_old"}; !slices.Equal(got, want) {
		t.Fatalf("StandingVerdicts() = %v, want %v", got, want)
	}
}
