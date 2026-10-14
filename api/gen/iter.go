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
