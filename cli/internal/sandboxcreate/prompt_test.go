package sandboxcreate

import (
	"context"
	"net/http"
	"strings"
	"testing"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

// fakeCreator answers with a name conflict until conflicts is exhausted,
// recording the name each attempt asked for.
type fakeCreator struct {
	conflicts int
	status    int
	names     []string
}

func (f *fakeCreator) CreateSandbox(_ context.Context, body *apimodel.CreateSandboxBody, _ apiclientgen.CreateSandboxParams) (apiclientgen.CreateSandboxRes, error) {
	f.names = append(f.names, body.Config.Name)
	if f.conflicts > 0 {
		f.conflicts--
		status := f.status
		if status == 0 {
			status = http.StatusConflict
		}
		return &apiclientgen.ErrorModelStatusCode{
			StatusCode: status,
			Response:   apiclientgen.ErrorModel{Detail: apiclientgen.NewOptString("a sandbox named x already exists in this project")},
		}, nil
	}
	return &apimodel.Sandbox{ID: "sbx_created0000001"}, nil
}

// TestCreatePromptSandboxRetriesAGeneratedNameConflict: the name here was
// generated, not chosen, so a collision is the CLI's to resolve rather than the
// user's to see.
func TestCreatePromptSandboxRetriesAGeneratedNameConflict(t *testing.T) {
	creator := &fakeCreator{conflicts: 2}
	sandbox, local, err := CreatePromptSandbox(context.Background(), creator, "project-1", PromptOptions{Source: newRunSourceTestRepo(t)})
	if err != nil {
		t.Fatalf("create prompt sandbox: %v", err)
	}
	defer local.Close()
	if sandbox.ID != "sbx_created0000001" {
		t.Fatalf("sandbox ID = %q", sandbox.ID)
	}
	if len(creator.names) != 3 {
		t.Fatalf("attempted %d names, want 3", len(creator.names))
	}
	for i, name := range creator.names {
		if strings.TrimSpace(name) == "" {
			t.Fatalf("attempt %d sent an empty name", i)
		}
		for j := 0; j < i; j++ {
			if creator.names[j] == name {
				t.Fatalf("retry reused the name %q that had just conflicted", name)
			}
		}
	}
}

// TestCreatePromptSandboxGivesUpOnAPersistentConflict keeps a broken server
// from becoming an infinite loop of sandbox creations.
func TestCreatePromptSandboxGivesUpOnAPersistentConflict(t *testing.T) {
	creator := &fakeCreator{conflicts: 100}
	_, _, err := CreatePromptSandbox(context.Background(), creator, "project-1", PromptOptions{Source: newRunSourceTestRepo(t)})
	if err == nil {
		t.Fatal("expected a persistent conflict to surface")
	}
	if len(creator.names) != nameConflictAttempts {
		t.Fatalf("made %d attempts, want %d", len(creator.names), nameConflictAttempts)
	}
}

// TestCreatePromptSandboxDoesNotRetryOtherFailures: only a name conflict is
// ours to fix by renaming.
func TestCreatePromptSandboxDoesNotRetryOtherFailures(t *testing.T) {
	creator := &fakeCreator{conflicts: 100, status: http.StatusBadRequest}
	if _, _, err := CreatePromptSandbox(context.Background(), creator, "project-1", PromptOptions{Source: newRunSourceTestRepo(t)}); err == nil {
		t.Fatal("expected the error to surface")
	}
	if len(creator.names) != 1 {
		t.Fatalf("made %d attempts, want 1: a non-conflict failure must not be retried", len(creator.names))
	}
}
