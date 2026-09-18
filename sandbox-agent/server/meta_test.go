package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/sandbox-agent/autostop"
	"github.com/discobox-ai/discobox/sandbox-agent/meta"
	"github.com/discobox-ai/discobox/sandbox-agent/ports"
	"github.com/discobox-ai/discobox/sandboxmeta"
)

func metaHandler(t *testing.T, home string) *handler {
	t.Helper()
	return &handler{
		ports:    ports.New(ports.Config{ProcRoot: t.TempDir()}),
		autostop: autostop.New(autostop.Config{LeaseDir: filepath.Join(t.TempDir(), "keepalive")}),
		meta:     meta.New(home, meta.Owner{}),
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
	if !ok || got.Tags == nil || len(got.Tags) != 0 || got.Description.Set {
		t.Fatalf("meta with no file = %+v (set %v), want empty meta", got, ok)
	}

	path := filepath.Join(home, sandboxmeta.RelativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"description": "fix the reaper", "tags": {"wip": ""}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err = agent.GetSandboxAgentStatus(ctx, sandboxapi.GetSandboxAgentStatusParams{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = status.Meta.Get()
	if got.Description.Or("") != "fix the reaper" || !reflect.DeepEqual(map[string]string(got.Tags), map[string]string{"wip": ""}) {
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
	if written.Meta.Description.Or("") != "fix the reaper" || written.Meta.Tags["wip"] != "" || written.ObservedAt.IsZero() {
		t.Fatalf("UpdateSandboxAgentMeta() = %+v", written)
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
