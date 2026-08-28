package sandboxcreate

import (
	"context"
	"net/http"
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
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
	sandbox, local, err := CreatePromptSandbox(context.Background(), creator, "project-1", PromptOptions{Source: newRunSourceTestRepo(t)}, nil)
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
	_, _, err := CreatePromptSandbox(context.Background(), creator, "project-1", PromptOptions{Source: newRunSourceTestRepo(t)}, nil)
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
	if _, _, err := CreatePromptSandbox(context.Background(), creator, "project-1", PromptOptions{Source: newRunSourceTestRepo(t)}, nil); err == nil {
		t.Fatal("expected the error to surface")
	}
	if len(creator.names) != 1 {
		t.Fatalf("made %d attempts, want 1: a non-conflict failure must not be retried", len(creator.names))
	}
}

// A discobox with no source materializes nothing, but is still one you started
// from here: the origin and the Git authorship come off the directory the
// command ran in, which is all Source still says.
func TestNoSourceCreatesWithNothingToMaterialize(t *testing.T) {
	repo := newRunSourceTestRepo(t)

	body, local, err := BuildPromptSandboxBody(context.Background(), PromptOptions{Source: repo, NoSource: true})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer local.Close()

	if _, ok := body.Config.Source.Get(); ok {
		t.Fatal("a sandbox with no source must send none")
	}
	if _, ok := body.Config.SourceCodeReferences.Get(); ok {
		t.Fatal("nothing was included, so there is nothing to reference")
	}
	origin, ok := body.Origin.Get()
	if !ok || origin.ProjectPath != repo {
		t.Fatalf("origin = %+v, want the directory the create came from (%s)", origin, repo)
	}
	if len(local.sources) != 0 {
		t.Fatalf("local sources = %+v, want nothing to deliver", local.sources)
	}
}

// --no-source still brings in what -i names: a discobox holding the extra
// sources and nothing else.
func TestNoSourceStillTakesIncludes(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	other := newRunSourceTestRepo(t)

	body, local, err := BuildPromptSandboxBody(context.Background(),
		PromptOptions{Source: repo, NoSource: true, Include: []string{other}, IncludeDirty: IncludeDirtyNever})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer local.Close()

	if _, ok := body.Config.Source.Get(); ok {
		t.Fatal("a sandbox with no source must send none")
	}
	references, ok := body.Config.SourceCodeReferences.Get()
	if !ok || len(references) != 1 {
		t.Fatalf("references = %+v, want the one -i named", references)
	}
}

// A ref names a commit to check out, and --no-source has nothing to check one
// out of. Saying both is a contradiction rather than a preference.
func TestNoSourceRefusesARef(t *testing.T) {
	_, _, err := BuildPromptSandboxBody(context.Background(),
		PromptOptions{Source: newRunSourceTestRepo(t), Ref: "main", NoSource: true})
	if err == nil || !strings.Contains(err.Error(), "no ref") {
		t.Fatalf("err = %v, want a refusal naming the ref", err)
	}
}
