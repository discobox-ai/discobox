package services

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy/egress"
	"github.com/discobox-ai/discobox/server/internal/model"
)

func TestSandboxToAPIIncludesRegisteredHarnessConfig(t *testing.T) {
	sandbox := &model.Sandbox{
		ID:        "sb_1",
		ProjectID: "p1",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState: model.DesiredStatePresent,
			State:        model.SandboxStateReady,
		},
		HarnessConfig: &model.HarnessConfig{
			ID:         "ac_1",
			ProjectID:  "p1",
			Slug:       "codex",
			BuiltIn:    true,
			Name:       "Codex",
			RunCommand: []string{"codex"},
		},
	}
	out, err := SandboxToAPI(sandbox, SandboxUpgradeBasis{})
	if err != nil {
		t.Fatalf("SandboxToAPI: %v", err)
	}
	harnessConfig, ok := out.HarnessConfig.Get()
	if !ok {
		t.Fatalf("expected embedded harness config")
	}
	if got := harnessConfig.RunCommand; len(got) != 1 || got[0] != "codex" {
		t.Fatalf("runCommand = %#v, want registered [codex]", got)
	}
}

func TestSandboxToAPIIncludesCalculatedDisplayState(t *testing.T) {
	sandbox := &model.Sandbox{
		ID:              "sb_1",
		ProjectID:       "p1",
		CreatedByUserID: "u1",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:       model.DesiredStatePresent,
			State:              model.SandboxStateReady,
			Generation:         2,
			ObservedGeneration: 1,
		},
		RuntimeState: model.SandboxRuntimeStateRunning,
	}
	out, err := SandboxToAPI(sandbox, SandboxUpgradeBasis{})
	if err != nil {
		t.Fatalf("SandboxToAPI: %v", err)
	}
	displayState, ok := out.Runtime.DisplayState.Get()
	if !ok {
		t.Fatal("displayState is missing")
	}
	// The observed runtime state is the answer, even mid-generation: the
	// sandbox is running, and a pending spec change does not make that untrue.
	if got := string(displayState); got != "running" {
		t.Fatalf("displayState = %q, want running", got)
	}
	runtimeState, ok := out.Runtime.RuntimeState.Get()
	if !ok {
		t.Fatal("runtimeState is missing")
	}
	if got := string(runtimeState); got != "running" {
		t.Fatalf("runtimeState = %q, want running", got)
	}
}

// A sandbox no agent has reported on serves no runtimeState at all. Empty is a
// member of the model's vocabulary and not of the API enum, so it is omitted
// rather than sent as "" (ADR 0034 §2).
func TestSandboxToAPIOmitsUnobservedRuntimeState(t *testing.T) {
	sandbox := &model.Sandbox{
		ID:              "sb_1",
		ProjectID:       "p1",
		CreatedByUserID: "u1",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState:       model.DesiredStatePresent,
			State:              model.SandboxStateReady,
			Generation:         1,
			ObservedGeneration: 1,
		},
	}
	out, err := SandboxToAPI(sandbox, SandboxUpgradeBasis{})
	if err != nil {
		t.Fatalf("SandboxToAPI: %v", err)
	}
	if _, ok := out.Runtime.RuntimeState.Get(); ok {
		t.Fatal("runtimeState is present for a sandbox nobody has reported on")
	}
	displayState, _ := out.Runtime.DisplayState.Get()
	if got := string(displayState); got != "starting" {
		t.Fatalf("displayState = %q, want starting", got)
	}
}

// A reported delivery is on the runtime, because a parked sandbox stays
// awaiting_source until the reconciler acts on the report, and a client
// deciding whether a delivery is still owed cannot tell the two apart from the
// state alone (ADR 26-09-24-005).
func TestSandboxToAPIReportsASourceDelivery(t *testing.T) {
	sandbox := &model.Sandbox{
		ID:              "sb_1",
		ProjectID:       "p1",
		CreatedByUserID: "u1",
		ResourceLifecycle: model.ResourceLifecycle{
			DesiredState: model.DesiredStatePresent,
			State:        model.SandboxStateAwaitingSource,
			Generation:   2,
		},
	}
	out, err := SandboxToAPI(sandbox, SandboxUpgradeBasis{})
	if err != nil {
		t.Fatalf("SandboxToAPI: %v", err)
	}
	if out.Runtime.SourceDeliveredAt.IsSet() {
		t.Fatal("sourceDeliveredAt is present before any delivery was reported")
	}

	delivered := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	sandbox.SourceDeliveredAt = &delivered
	out, err = SandboxToAPI(sandbox, SandboxUpgradeBasis{})
	if err != nil {
		t.Fatalf("SandboxToAPI: %v", err)
	}
	if got, ok := out.Runtime.SourceDeliveredAt.Get(); !ok || !got.Equal(delivered) {
		t.Fatalf("sourceDeliveredAt = %v (set %v), want %v", got, ok, delivered)
	}
}

// The primary's source data is mounted under "primary" whatever the primary's
// own slug, and in a sandbox with no primary at all, so no reference is given
// that name -- not even one whose key asks for it.
func TestDefaultGitSourceSlugsReservePrimaryForThePrimary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		primary *model.GitSource
	}{
		{name: "no primary"},
		{name: "a primary with its own slug", primary: &model.GitSource{Slug: new("app")}},
		{name: "an unnamed primary", primary: &model.GitSource{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refs := model.SourceCodeReferences{"primary": {}, "lib": {Slug: new("primary")}}
			DefaultGitSourceSlugs(tc.primary, refs)
			for key, ref := range refs {
				if ref.Slug == nil || *ref.Slug == "primary" {
					t.Errorf("reference %q slug = %v, want anything but primary", key, ref.Slug)
				}
			}
			if tc.primary != nil && tc.primary.Slug == nil {
				t.Fatal("primary was given no slug")
			}
		})
	}
}

// Who made a discobox reaches the API: the user always, and the sandbox when
// one did (ADR 26-09-24-630 §1). A person's discobox says no sandbox at all.
func TestSandboxToAPIIncludesTheCreatingSandbox(t *testing.T) {
	lead := "sbx-lead"
	for _, tc := range []struct {
		createdBy *string
		want      string
	}{{&lead, lead}, {nil, ""}} {
		out, err := SandboxToAPI(&model.Sandbox{ID: "sb_1", ProjectID: "p1", CreatedByUserID: "user-1", CreatedBySandboxID: tc.createdBy}, SandboxUpgradeBasis{})
		if err != nil {
			t.Fatalf("SandboxToAPI: %v", err)
		}
		got, set := out.CreatedBySandboxId.Get()
		if got != tc.want || set != (tc.want != "") {
			t.Fatalf("createdBySandboxId = %q (set %t), want %q", got, set, tc.want)
		}
	}
}

// A Windows sandbox has one account and no ids, so its create may name that
// account and nothing more. A uid or a group set is refused as a bad request
// saying why, never dropped on the way to a sandbox that cannot honor it (ADR
// 0145 §5).
func TestSandboxUserValidateAccountRefusesIDsOnWindows(t *testing.T) {
	name, uid := "ada", 501
	if err := (SandboxUserFields{Name: &name}).ValidateAccount("windows"); err != nil {
		t.Fatalf("the account by name = %v, want nil", err)
	}
	for _, tc := range []struct {
		fields SandboxUserFields
		want   string
	}{
		{SandboxUserFields{Name: &name, UID: &uid}, "a uid (501)"},
		{SandboxUserFields{AdditionalGroups: []string{"staff"}}, "a group set (staff)"},
	} {
		err := tc.fields.ValidateAccount("windows")
		var statusErr interface{ StatusCode() int }
		if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusBadRequest ||
			!strings.Contains(err.Error(), "runs every process as it") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidateAccount(%+v) = %v, want a 400 naming %q and why", tc.fields, err, tc.want)
		}
	}
}

// A macOS client's own account, 501 in staff, is one a darwin sandbox is
// created with and a Linux one refuses as a bad request (ADR 26-11-12-060).
func TestSandboxUserValidateAccountTakesTheGuestsIDs(t *testing.T) {
	name, uid, gid := "ada", 501, 20
	macAccount := SandboxUserFields{Name: &name, UID: &uid, GID: &gid}
	if err := macAccount.ValidateAccount("darwin"); err != nil {
		t.Fatalf("501/20 on darwin = %v, want nil", err)
	}
	err := macAccount.ValidateAccount("linux")
	var statusErr interface{ StatusCode() int }
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != http.StatusBadRequest || !strings.Contains(err.Error(), "uid 501") {
		t.Fatalf("501/20 on linux = %v, want a 400 naming uid 501", err)
	}
	linuxUID := 1000
	if err := (SandboxUserFields{Name: &name, UID: &linuxUID}).ValidateAccount("linux"); err != nil {
		t.Fatalf("a Linux account = %v, want nil", err)
	}
}

// The sandbox's own egress level is on its record: `box update` keeps the part
// of a level its flags leave out by reading it back from here.
func TestSandboxToAPIIncludesItsEgressLevel(t *testing.T) {
	sandbox := &model.Sandbox{ID: "sb_1", ProjectID: "p1", Egress: egress.Policy{DenyByDefault: true, Allow: []string{"github.com"}}}
	out, err := SandboxToAPI(sandbox, SandboxUpgradeBasis{})
	if err != nil {
		t.Fatalf("SandboxToAPI: %v", err)
	}
	level, ok := out.Egress.Get()
	if !ok || !level.DenyByDefault.Or(false) {
		t.Fatalf("egress = %+v, set %v", level, ok)
	}
	if allow, _ := level.Allow.Get(); len(allow) != 1 || allow[0] != "github.com" {
		t.Fatalf("allow = %v", allow)
	}
}
