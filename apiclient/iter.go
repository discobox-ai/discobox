package apiclient

import (
	"iter"

	"github.com/obot-platform/discobox/api/model"
)

func Projects(body *model.ListProjectsBody) iter.Seq[model.Project] {
	return func(yield func(model.Project) bool) {
		if body == nil {
			return
		}
		for _, project := range body.Projects {
			if !yield(project) {
				return
			}
		}
	}
}

func Sandboxes(body *model.ListSandboxesBody) iter.Seq[model.Sandbox] {
	return func(yield func(model.Sandbox) bool) {
		if body == nil {
			return
		}
		for _, sandbox := range body.Sandboxes {
			if !yield(sandbox) {
				return
			}
		}
	}
}
