package secretroute

import "testing"

func TestProveContained(t *testing.T) {
	routes := func(texts []string) []Route {
		var out []Route
		for _, s := range texts {
			r, e := Parse(s)
			if e != nil {
				t.Fatal(e)
			}
			out = append(out, r)
		}
		return out
	}
	for _, tc := range []struct {
		name         string
		grant, limit []string
		ok           bool
	}{
		{"same policy with exception", []string{"api.github.com/repos/acme/**", "DELETE api.github.com/repos/acme/web=blocked"}, []string{"DELETE api.github.com/repos/acme/web=blocked", "api.github.com/repos/acme/**"}, true},
		{"removed exception", []string{"api.github.com/repos/acme/**"}, []string{"api.github.com/repos/acme/**", "DELETE api.github.com/repos/acme/web=blocked"}, false},
		{"narrow repository", []string{"GET api.github.com/repos/acme/web/**"}, []string{"GET,POST *.github.com/repos/acme/**"}, true},
		{"wide host", []string{"GET *.github.com/repos/acme/web/**"}, []string{"GET api.github.com/repos/acme/**"}, false},
		{"wide method", []string{"api.github.com/repos/acme/**"}, []string{"GET api.github.com/repos/acme/**"}, false},
		{"wide path", []string{"GET api.github.com/repos/**"}, []string{"GET api.github.com/repos/acme/**"}, false},
		{"graphql narrowing", []string{"POST api.github.com/graphql graphql=query:repository"}, []string{"POST api.github.com/graphql graphql=query:repo*"}, true},
		{"graphql widening", []string{"POST api.github.com/graphql graphql=mutation"}, []string{"POST api.github.com/graphql graphql=query"}, false},
		{"mcp narrowing", []string{"tools.example/mcp mcp=read_file"}, []string{"tools.example/mcp mcp=read_*"}, true},
		{"mcp widening", []string{"tools.example/mcp mcp=*"}, []string{"tools.example/mcp mcp=read_*"}, false},
		{"deny only", []string{"*=blocked"}, []string{"api.github.com"}, true},
		{"exception changed", []string{"api.github.com", "DELETE api.github.com/other=blocked"}, []string{"api.github.com", "DELETE api.github.com/repos/acme/web=blocked"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ProveContained(routes([]string{"*"}), routes(tc.grant), routes(tc.limit))
			if (err == nil) != tc.ok {
				t.Fatalf("proof = %v, want %v", err, tc.ok)
			}
		})
	}
}
