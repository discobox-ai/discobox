package apiclient

import (
	"testing"

	"github.com/obot-platform/discobox/api/model"
)

func TestProjectsSeq(t *testing.T) {
	first := "11111111-1111-1111-1111-111111111111"
	second := "22222222-2222-2222-2222-222222222222"
	body := &model.ListProjectsBody{
		Projects: []model.Project{
			{ID: first},
			{ID: second},
		},
	}

	var got []string
	for project := range Projects(body) {
		got = append(got, project.ID)
	}
	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("projects = %#v", got)
	}
}
