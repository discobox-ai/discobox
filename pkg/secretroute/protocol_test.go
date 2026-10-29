package secretroute

import (
	"net/url"
	"testing"
)

// post is a request carrying body.
func post(rawURL, body string) Request {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	return Request{Method: "POST", Host: u.Hostname(), URL: rawURL, Body: func() ([]byte, bool) { return []byte(body), true }}
}

func TestGraphQL(t *testing.T) {
	const endpoint = "https://api.github.com/graphql"
	type check struct {
		routes []string
		want   bool
	}
	queries := []string{"api.github.com/graphql graphql=query"}
	mutations := []string{"api.github.com/graphql graphql=mutation:createPullRequest,add*"}
	carveOut := []string{"api.github.com/graphql", "api.github.com/graphql graphql=mutation:merge*,delete*=blocked"}
	for _, tc := range []struct {
		name   string
		req    Request
		checks []check
	}{
		{
			name: "a query",
			req:  post(endpoint, `{"query":"query { viewer { login } }"}`),
			checks: []check{
				{queries, true}, {mutations, false}, {carveOut, true},
				{[]string{"api.github.com/graphql graphql=*:viewer"}, true},
				{[]string{"api.github.com/graphql graphql=*:repository"}, false},
			},
		},
		{
			name:   "a shorthand query is a query",
			req:    post(endpoint, `{"query":"{ viewer { login } }"}`),
			checks: []check{{queries, true}, {mutations, false}},
		},
		{
			name:   "a mutation the route names",
			req:    post(endpoint, `{"query":"mutation { createPullRequest(input: {}) { clientMutationId } }"}`),
			checks: []check{{queries, false}, {mutations, true}, {carveOut, true}},
		},
		{
			name:   "a mutation by prefix",
			req:    post(endpoint, `{"query":"mutation { addComment(input: {}) { clientMutationId } }"}`),
			checks: []check{{mutations, true}},
		},
		{
			name:   "a mutation the block refuses",
			req:    post(endpoint, `{"query":"mutation { mergePullRequest(input: {}) { clientMutationId } }"}`),
			checks: []check{{mutations, false}, {carveOut, false}},
		},
		{
			name: "an alias is read as its field",
			req:  post(endpoint, `{"query":"mutation { createPullRequest: mergePullRequest(input: {}) { clientMutationId } }"}`),
			checks: []check{
				{mutations, false}, {carveOut, false},
			},
		},
		{
			name:   "every root field must match an open route",
			req:    post(endpoint, `{"query":"mutation { createPullRequest(input: {}) { x } mergePullRequest(input: {}) { x } }"}`),
			checks: []check{{mutations, false}, {carveOut, false}},
		},
		{
			name:   "__typename is ignored",
			req:    post(endpoint, `{"query":"mutation { __typename createPullRequest(input: {}) { x } }"}`),
			checks: []check{{mutations, true}},
		},
		{
			name: "root fragment spreads are expanded",
			req:  post(endpoint, `{"query":"mutation { ...M } fragment M on Mutation { ...N } fragment N on Mutation { deleteRepository(input: {}) { x } }"}`),
			checks: []check{
				{[]string{"api.github.com/graphql graphql=mutation:createPullRequest"}, false}, {carveOut, false},
			},
		},
		{
			name:   "inline fragments are expanded",
			req:    post(endpoint, `{"query":"mutation { ... on Mutation { deleteRepository(input: {}) { x } } }"}`),
			checks: []check{{mutations, false}, {carveOut, false}},
		},
		{
			name:   "an undefined fragment is unknown",
			req:    post(endpoint, `{"query":"query { ...Missing }"}`),
			checks: []check{{queries, false}, {carveOut, false}},
		},
		{
			name:   "a fragment that spreads itself is unknown",
			req:    post(endpoint, `{"query":"query { ...A } fragment A on Query { ...A }"}`),
			checks: []check{{queries, false}, {carveOut, false}},
		},
		{
			name:   "a fragment defined twice is unknown",
			req:    post(endpoint, `{"query":"query { ...A } fragment A on Query { viewer { id } } fragment A on Mutation { deleteRepository { x } }"}`),
			checks: []check{{queries, false}, {carveOut, false}},
		},
		{
			name: "operationName is never read",
			req:  post(endpoint, `{"operationName":"Q","query":"query Q { viewer { id } } mutation M { deleteRepository(input: {}) { x } }"}`),
			checks: []check{
				{queries, false}, {carveOut, false},
			},
		},
		{
			name:   "a batch matches an open route only if every operation does",
			req:    post(endpoint, `[{"query":"{ viewer { id } }"},{"query":"query { repository { id } }"}]`),
			checks: []check{{queries, true}, {carveOut, true}},
		},
		{
			name:   "a batch with one mutation",
			req:    post(endpoint, `[{"query":"{ viewer { id } }"},{"query":"mutation { deleteRepository(input: {}) { x } }"}]`),
			checks: []check{{queries, false}, {carveOut, false}},
		},
		{
			name:   "a persisted query with no document",
			req:    post(endpoint, `{"extensions":{"persistedQuery":{"version":1,"sha256Hash":"abc"}}}`),
			checks: []check{{queries, false}, {carveOut, false}},
		},
		{
			name:   "a null query",
			req:    post(endpoint, `{"query":null}`),
			checks: []check{{queries, false}, {carveOut, false}},
		},
		{
			name:   "bad JSON",
			req:    post(endpoint, `{"query":"{ viewer { id } }"`),
			checks: []check{{queries, false}, {carveOut, false}},
		},
		{
			name:   "trailing data",
			req:    post(endpoint, `{"query":"{ viewer { id } }"} {"query":"mutation { deleteRepository { x } }"}`),
			checks: []check{{queries, false}, {carveOut, false}},
		},
		{
			name:   "a document that does not parse",
			req:    post(endpoint, `{"query":"mutation { deleteRepository("}`),
			checks: []check{{queries, false}, {carveOut, false}},
		},
		{
			name:   "a duplicate query key",
			req:    post(endpoint, `{"query":"{ viewer { id } }","query":"mutation { deleteRepository { x } }"}`),
			checks: []check{{queries, false}, {carveOut, false}},
		},
		{
			name:   "a query key in another case",
			req:    post(endpoint, `{"query":"{ viewer { id } }","Query":"mutation { deleteRepository { x } }"}`),
			checks: []check{{queries, false}, {carveOut, false}},
		},
		{
			name:   "a query key only in another case",
			req:    post(endpoint, `{"QUERY":"mutation { deleteRepository { x } }"}`),
			checks: []check{{queries, false}, {carveOut, false}},
		},
		{
			name:   "a body that is not JSON",
			req:    post(endpoint, `mutation { deleteRepository { x } }`),
			checks: []check{{queries, false}, {carveOut, false}},
		},
		{
			name:   "an empty batch",
			req:    post(endpoint, `[]`),
			checks: []check{{queries, false}, {carveOut, false}},
		},
		{
			name:   "an empty body executes nothing",
			req:    post(endpoint, ``),
			checks: []check{{queries, false}, {carveOut, true}},
		},
		{
			name:   "a GET query in the URL",
			req:    post(endpoint+"?query="+url.QueryEscape("{ viewer { id } }"), ``),
			checks: []check{{queries, true}, {carveOut, true}},
		},
		{
			name:   "a mutation in the URL beside a query in the body",
			req:    post(endpoint+"?query="+url.QueryEscape("mutation { deleteRepository { x } }"), `{"query":"{ viewer { id } }"}`),
			checks: []check{{queries, false}, {carveOut, false}},
		},
		{
			name:   "a persisted query in the URL",
			req:    post(endpoint+"?extensions=%7B%7D", ``),
			checks: []check{{queries, false}, {carveOut, false}},
		},
		{
			name:   "no body available",
			req:    Request{Method: "POST", Host: "api.github.com", URL: endpoint},
			checks: []check{{queries, false}, {carveOut, false}},
		},
		{
			name: "a body past the limit, or in another encoding",
			req: Request{Method: "POST", Host: "api.github.com", URL: endpoint, Body: func() ([]byte, bool) {
				return []byte(`{"query":"{ viewer { id } }"}`), false
			}},
			checks: []check{{queries, false}, {carveOut, false}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, c := range tc.checks {
				allowed, reason := Decide([][]Route{routes(t, c.routes...)}, tc.req)
				if allowed != c.want {
					t.Fatalf("Decide(%v) = %v (%s), want %v", c.routes, allowed, reason, c.want)
				}
			}
		})
	}
}

func TestMCP(t *testing.T) {
	const endpoint = "https://api.githubcopilot.com/mcp"
	type check struct {
		routes []string
		want   bool
	}
	readOnly := []string{"api.githubcopilot.com/mcp mcp=get_*,search_code"}
	anyTool := []string{"api.githubcopilot.com/mcp mcp=*"}
	carveOut := []string{"api.githubcopilot.com/mcp", "api.githubcopilot.com/mcp mcp=delete_*,merge_*=blocked"}
	call := func(name string) string {
		return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":{}}}`
	}
	for _, tc := range []struct {
		name       string
		body       string
		method     string // POST when empty
		unreadable bool   // Body returns false
		checks     []check
	}{
		{name: "a tool the route names", body: call("search_code"), checks: []check{{readOnly, true}, {anyTool, true}, {carveOut, true}}},
		{name: "a tool by prefix", body: call("get_file_contents"), checks: []check{{readOnly, true}}},
		{name: "a tool the route does not name", body: call("create_issue"), checks: []check{{readOnly, false}, {anyTool, true}, {carveOut, true}}},
		{name: "a tool the block refuses", body: call("merge_pull_request"), checks: []check{{readOnly, false}, {anyTool, true}, {carveOut, false}}},
		{
			name:   "protocol methods match any MCP route",
			body:   `[{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"tools/list"},{"jsonrpc":"2.0","id":3,"method":"ping"}]`,
			checks: []check{{readOnly, true}, {anyTool, true}, {carveOut, true}},
		},
		{name: "another method", body: `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"x"}}`, checks: []check{{readOnly, false}, {anyTool, false}, {carveOut, true}}},
		{name: "a response to the server's request", body: `{"jsonrpc":"2.0","id":1,"result":{"roots":[]}}`, checks: []check{{readOnly, true}, {anyTool, true}, {carveOut, true}}},
		{name: "an error response", body: `{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"no"}}`, checks: []check{{readOnly, true}, {carveOut, true}}},
		{name: "a response without a result or error", body: `{"jsonrpc":"2.0","id":1}`, checks: []check{{readOnly, false}, {carveOut, false}}},
		{name: "responses beside an allowed call", body: `[{"jsonrpc":"2.0","id":1,"result":{}},` + call("get_me") + "]", checks: []check{{readOnly, true}, {carveOut, true}}},
		{name: "responses beside a refused call", body: `[{"jsonrpc":"2.0","id":1,"result":{}},` + call("delete_file") + "]", checks: []check{{readOnly, false}, {anyTool, true}, {carveOut, false}}},
		{name: "a batch of allowed calls", body: "[" + call("get_me") + "," + call("search_code") + "]", checks: []check{{readOnly, true}, {carveOut, true}}},
		{name: "a batch with one refused call", body: "[" + call("get_me") + "," + call("delete_file") + "]", checks: []check{{readOnly, false}, {anyTool, true}, {carveOut, false}}},
		{name: "a call without a name", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`, checks: []check{{anyTool, false}, {carveOut, false}}},
		{name: "a name in another case", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_me","Name":"delete_file"}}`, checks: []check{{readOnly, false}, {carveOut, false}}},
		{name: "a duplicate name", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_me","name":"delete_file"}}`, checks: []check{{readOnly, false}, {carveOut, false}}},
		{name: "a method in another case", body: `{"jsonrpc":"2.0","id":1,"method":"ping","METHOD":"tools/call","params":{"name":"delete_file"}}`, checks: []check{{readOnly, false}, {carveOut, false}}},
		{name: "not JSON-RPC 2.0", body: `{"id":1,"method":"tools/call","params":{"name":"get_me"}}`, checks: []check{{readOnly, false}, {carveOut, false}}},
		{name: "bad JSON", body: `{"jsonrpc":"2.0",`, checks: []check{{readOnly, false}, {carveOut, false}}},
		{name: "invalid UTF-8", body: "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"get_\xff\"}}", checks: []check{{readOnly, false}, {carveOut, false}}},
		{name: "an empty POST", body: ``, checks: []check{{anyTool, false}, {carveOut, true}}},
		{name: "a GET opening the server's stream", method: "GET", body: ``, checks: []check{{readOnly, true}, {anyTool, true}, {carveOut, true}}},
		{name: "a DELETE ending the session", method: "DELETE", body: ``, checks: []check{{readOnly, true}, {carveOut, true}}},
		{name: "a GET carrying a call", method: "GET", body: call("delete_file"), checks: []check{{readOnly, false}, {carveOut, false}}},
		{name: "a GET whose body cannot be read", method: "GET", unreadable: true, checks: []check{{readOnly, false}, {carveOut, false}}},
		{name: "a PUT with an empty body", method: "PUT", body: ``, checks: []check{{readOnly, false}, {carveOut, true}}},
		{name: "a body past the limit", body: call("get_me"), unreadable: true, checks: []check{{readOnly, false}, {anyTool, false}, {carveOut, false}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := post(endpoint, tc.body)
			req.Body = func() ([]byte, bool) { return []byte(tc.body), !tc.unreadable }
			if tc.method != "" {
				req.Method = tc.method
			}
			for _, c := range tc.checks {
				allowed, reason := Decide([][]Route{routes(t, c.routes...)}, req)
				if allowed != c.want {
					t.Fatalf("Decide(%v) = %v (%s), want %v", c.routes, allowed, reason, c.want)
				}
			}
		})
	}
	// No body at all matches no MCP route, and every blocked one.
	nilBody := Request{Method: "POST", Host: "api.githubcopilot.com", URL: endpoint}
	if allowed, _ := Decide([][]Route{routes(t, anyTool...)}, nilBody); allowed {
		t.Fatal("an MCP route matched a request with no body available")
	}
	if allowed, _ := Decide([][]Route{routes(t, carveOut...)}, nilBody); allowed {
		t.Fatal("a blocked MCP route was stepped around by a body that could not be read")
	}
}
