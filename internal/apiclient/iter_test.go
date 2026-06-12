package apiclient

import (
	"testing"

	"github.com/google/uuid"
	apiclientgen "github.com/obot-platform/discobox/internal/apiclient/gen"
)

func TestProjectsSeq(t *testing.T) {
	first := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	second := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	body := &apiclientgen.ListProjectsBody{
		Projects: []apiclientgen.Project{
			{ID: first},
			{ID: second},
		},
	}

	var got []uuid.UUID
	for project := range Projects(body) {
		got = append(got, project.ID)
	}
	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("projects = %#v", got)
	}
}
