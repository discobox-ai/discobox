package cli

import (
	"context"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/internal/hostid"
)

// A directory brought in with no repository of its own (-i notes/) has no
// history to send, so pushing every source skips it, the way a remote source is
// skipped, rather than failing the whole push.
func TestPushSkipsASourceCreatedFromNoRepositoryUnlessNamed(t *testing.T) {
	host, err := hostid.Get()
	if err != nil {
		t.Fatal(err)
	}
	notes := t.TempDir()
	source := apiclientgen.GitSource{Kind: apiclientgen.GitSourceKindGit}
	source.SetSlug(apiclientgen.NewOptString("notes"))
	source.SetLocalDirectory(apiclientgen.NewOptString(notes))
	source.SetNoLocalRepository(apiclientgen.NewOptBool(true))
	sandbox := &apimodel.Sandbox{}
	sandbox.SetOrigin(apiclientgen.NewOptOrigin(apiclientgen.Origin{HostId: host}))
	entry := sourceEntry{slug: "notes", source: source}

	got := pushOneSource(context.Background(), "project-1", pushTestSandboxID, sandbox, host, "http://127.0.0.1:1", "", entry, nil, "", false, false)
	if got.Status != pushStatusSkipped {
		t.Fatalf("push of every source: notes = %s (%s), want %s", got.Status, got.Error, pushStatusSkipped)
	}
	if got = pushOneSource(context.Background(), "project-1", pushTestSandboxID, sandbox, host, "http://127.0.0.1:1", "", entry, nil, "", false, true); got.Status != pushStatusError {
		t.Fatalf("push --source notes = %s, want %s", got.Status, pushStatusError)
	}
}
