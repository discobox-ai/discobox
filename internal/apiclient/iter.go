package apiclient

import (
	"iter"

	apiclientgen "github.com/obot-platform/discobox/internal/apiclient/gen"
)

func Projects(body *apiclientgen.ListProjectsBody) iter.Seq[apiclientgen.Project] {
	return func(yield func(apiclientgen.Project) bool) {
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

func Sandboxes(body *apiclientgen.ListSandboxesBody) iter.Seq[apiclientgen.Sandbox] {
	return func(yield func(apiclientgen.Sandbox) bool) {
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
