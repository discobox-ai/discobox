package sandboxes

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

// A create's description and tags are recorded with the sandbox from the
// start, so its first listing carries them and its created event the tags
// (an event has no description, ADR 26-10-29-316 §1), with no second call, and the reconciler hands them to the pool as the seed for the meta
// file. They stay unobserved, which the sandbox's first report replaces.
func TestCreateRecordsItsDescriptionAndTags(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	svc.SetReleaseBundles(release(platformOld))
	tags := map[string]string{"repo": "a/b", "job": "deliver", "wip": ""}
	created, err := svc.CreateSandbox(ctx, "project-1", services.CreateSandboxBody{
		HarnessName: serverapi.NewOptString("codex"),
		Config: serverapi.SandboxCreateConfig{
			Name:        "tagged",
			Description: serverapi.NewOptString("deliver 700"),
			Tags:        serverapi.NewOptSandboxCreateConfigTags(serverapi.SandboxCreateConfigTags(tags)),
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sb, err := st.GetSandbox(ctx, "project-1", created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(sb.Tags, tags) || sb.Description == nil || *sb.Description != "deliver 700" || sb.MetaObservedAt != nil {
		t.Fatalf("row = tags %v, description %v, observed %v; want the create's, unobserved", sb.Tags, sb.Description, sb.MetaObservedAt)
	}
	listed, err := svc.ListSandboxes(ctx, "project-1", "", nil, nil, "")
	if err != nil || len(listed) != 1 || !maps.Equal(listed[0].Tags, tags) {
		t.Fatalf("listing = %+v, %v; want the sandbox with its tags", listed, err)
	}

	events, err := st.ListSandboxEventsAfter(ctx, "project-1", 0, 10)
	if err != nil || len(events) == 0 {
		t.Fatalf("events = %v, %v; want the created event", events, err)
	}
	if events[0].Type != model.SandboxEventCreated {
		t.Fatalf("first event = %s, want %s", events[0].Type, model.SandboxEventCreated)
	}
	var data model.SandboxEventData
	if err := json.Unmarshal(events[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(data.Tags, tags) {
		t.Fatalf("created event tags = %v, want the create's", data.Tags)
	}

	opts, err := (&SandboxReconciler{store: st}).createOptionsFromSandbox(ctx, sb)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(opts.Tags, tags) || opts.Description == nil || *opts.Description != "deliver 700" {
		t.Fatalf("seed for the pool = tags %v, description %v; want the create's", opts.Tags, opts.Description)
	}
}

// A description or tags a meta change could not make are refused before
// anything is created.
func TestCreateRefusesMetaAChangeWouldRefuse(t *testing.T) {
	ctx, svc, st, _ := transferFixture(t)
	configuredHarness(t, st, "codex", "Codex")
	svc.SetReleaseBundles(release(platformOld))
	for name, config := range map[string]serverapi.SandboxCreateConfig{
		"a key with a space":   {Name: "a", Tags: serverapi.NewOptSandboxCreateConfigTags(serverapi.SandboxCreateConfigTags{"bad key": "x"})},
		"a value with a comma": {Name: "b", Tags: serverapi.NewOptSandboxCreateConfigTags(serverapi.SandboxCreateConfigTags{"k": "a,b"})},
		"a control character":  {Name: "c", Description: serverapi.NewOptString("a\x07b")},
		"an empty key":         {Name: "d", Tags: serverapi.NewOptSandboxCreateConfigTags(serverapi.SandboxCreateConfigTags{"": "x"})},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CreateSandbox(ctx, "project-1", services.CreateSandboxBody{HarnessName: serverapi.NewOptString("codex"), Config: config})
			var status interface{ StatusCode() int }
			if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
				t.Fatalf("err = %v, want a 400", err)
			}
		})
	}
	listed, err := svc.ListSandboxes(ctx, "project-1", "", nil, nil, "")
	if err != nil || len(listed) != 0 {
		t.Fatalf("listing = %+v, %v; want nothing created", listed, err)
	}
}
