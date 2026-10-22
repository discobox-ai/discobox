package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/pkg/sandboxmeta"
	"github.com/discobox-ai/discobox/sandbox-agent/autostop"
	"github.com/discobox-ai/discobox/sandbox-agent/meta"
	"github.com/discobox-ai/discobox/sandbox-agent/ports"
)

func metaHandler(t *testing.T, home string) *handler {
	t.Helper()
	return &handler{
		ports:           ports.New(ports.Config{Scanner: ports.Procfs{Root: t.TempDir()}}),
		autostop:        autostop.New(autostop.Config{LeaseDir: filepath.Join(t.TempDir(), "keepalive")}),
		meta:            meta.New(home, meta.Owner{}),
		resourceSampler: emptyResources(t),
	}
}

// The status report carries what the meta file holds, so an edit made inside
// the sandbox reaches the control plane's copy, and a file that does not read
// is reported as such rather than as empty meta (ADR 0136).
func TestGetSandboxAgentStatusReportsMeta(t *testing.T) {
	home := t.TempDir()
	agent := metaHandler(t, home)
	ctx := context.Background()

	status, err := agent.GetSandboxAgentStatus(ctx, sandboxapi.GetSandboxAgentStatusParams{})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := status.Meta.Get()
	// Nothing is set but the tags, which are sent empty rather than left out
	// for a control plane from before they were optional.
	if tags, sent := got.Tags.Get(); !ok || !sent || len(tags) != 0 || got.Description.Set || got.Name.Set {
		t.Fatalf("meta with no file = %+v (set %v), want empty tags and nothing else", got, ok)
	}

	path := filepath.Join(home, sandboxmeta.RelativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"name": "the reaper", "description": "fix the reaper", "tags": {"wip": ""}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err = agent.GetSandboxAgentStatus(ctx, sandboxapi.GetSandboxAgentStatusParams{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = status.Meta.Get()
	if got.Name.Or("") != "the reaper" || got.Description.Or("") != "fix the reaper" || !reflect.DeepEqual(map[string]string(got.Tags.Or(nil)), map[string]string{"wip": ""}) {
		t.Fatalf("meta = %+v, want what the file holds", got)
	}

	if err := os.WriteFile(path, []byte(`{"tags": ["wip"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err = agent.GetSandboxAgentStatus(ctx, sandboxapi.GetSandboxAgentStatusParams{})
	if err != nil {
		t.Fatal(err)
	}
	if status.Meta.Set || !status.MetaError.Set {
		t.Fatalf("an invalid file reported meta %+v and error %q; want only the error", status.Meta, status.MetaError.Or(""))
	}
}

func TestUpdateSandboxAgentMeta(t *testing.T) {
	home := t.TempDir()
	agent := metaHandler(t, home)
	ctx := context.Background()

	written, err := agent.UpdateSandboxAgentMeta(ctx, &sandboxapi.UpdateSandboxMetaBody{
		Description: sandboxapi.NewOptString("fix the reaper"),
		SetTags:     sandboxapi.NewOptUpdateSandboxMetaBodySetTags(sandboxapi.UpdateSandboxMetaBodySetTags{"wip": ""}),
	}, sandboxapi.UpdateSandboxAgentMetaParams{})
	if err != nil {
		t.Fatalf("UpdateSandboxAgentMeta() error = %v", err)
	}
	if _, tagged := written.Meta.Tags.Or(nil)["wip"]; written.Meta.Description.Or("") != "fix the reaper" || !tagged || written.ObservedAt.IsZero() {
		t.Fatalf("UpdateSandboxAgentMeta() = %+v", written)
	}

	// A rename is the same write (ADR 26-10-12-026): the name is trimmed and
	// kept in the file with what was already there, and an empty one clears it.
	written, err = agent.UpdateSandboxAgentMeta(ctx, &sandboxapi.UpdateSandboxMetaBody{Name: sandboxapi.NewOptString(" Daily GH Check In ")}, sandboxapi.UpdateSandboxAgentMetaParams{})
	if err != nil || written.Meta.Name.Or("") != "Daily GH Check In" || written.Meta.Description.Or("") != "fix the reaper" {
		t.Fatalf("a rename answered %+v, %v; want the name beside the description", written, err)
	}
	onDisk, err := os.ReadFile(filepath.Join(home, sandboxmeta.RelativePath))
	if err != nil || !strings.Contains(string(onDisk), `"name": "Daily GH Check In"`) {
		t.Fatalf("the meta file holds %q, %v; want the name in it", onDisk, err)
	}
	written, err = agent.UpdateSandboxAgentMeta(ctx, &sandboxapi.UpdateSandboxMetaBody{Name: sandboxapi.NewOptString("")}, sandboxapi.UpdateSandboxAgentMetaParams{})
	if err != nil || written.Meta.Name.Set {
		t.Fatalf("clearing the name answered %+v, %v", written, err)
	}

	_, err = agent.UpdateSandboxAgentMeta(ctx, &sandboxapi.UpdateSandboxMetaBody{RemoveTags: []string{"a=b"}}, sandboxapi.UpdateSandboxAgentMetaParams{})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("an invalid change error = %v, want a 400", err)
	}

	if err := os.WriteFile(filepath.Join(home, sandboxmeta.RelativePath), []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = agent.UpdateSandboxAgentMeta(ctx, &sandboxapi.UpdateSandboxMetaBody{}, sandboxapi.UpdateSandboxAgentMetaParams{})
	if !errors.As(err, &status) || status.StatusCode() != http.StatusConflict {
		t.Fatalf("an invalid file error = %v, want a 409", err)
	}
}
