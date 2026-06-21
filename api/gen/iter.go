package apigen

import "iter"

func Projects(body *ListProjectsBody) iter.Seq[Project] {
	return func(yield func(Project) bool) {
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

func Sandboxes(body *ListSandboxesBody) iter.Seq[Sandbox] {
	return func(yield func(Sandbox) bool) {
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
