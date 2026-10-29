package secretroute

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      Route
		want    Route
		wantErr string
	}{
		{
			name: "lower-cases, sorts and dedupes",
			in: Route{
				Hosts:   []string{"GitHub.com:443", "*.GitHub.com", "github.com"},
				Methods: []string{"post", " GET", "POST"},
				Paths:   []string{"/b/", "/a/**", "/b"},
			},
			want: Route{Hosts: []string{"*.github.com", "github.com"}, Methods: []string{"GET", "POST"}, Paths: []string{"/a/**", "/b"}},
		},
		{name: "any host", in: Route{Hosts: []string{"*"}}, want: Route{Hosts: []string{"*"}}},
		{name: "root path stays", in: Route{Hosts: []string{"x.test"}, Paths: []string{"/"}}, want: Route{Hosts: []string{"x.test"}, Paths: []string{"/"}}},
		{
			name: "graphql operations lower-cased, fields sorted",
			in:   Route{Hosts: []string{"x.test"}, GraphQL: &GraphQL{Operations: []string{"Mutation", "query", "query"}, Fields: []string{"viewer", "create*"}}},
			want: Route{Hosts: []string{"x.test"}, GraphQL: &GraphQL{Operations: []string{"mutation", "query"}, Fields: []string{"create*", "viewer"}}},
		},
		{
			name: "a * field or tool is every one",
			in:   Route{Hosts: []string{"x.test"}, GraphQL: &GraphQL{Fields: []string{"viewer", "*"}}},
			want: Route{Hosts: []string{"x.test"}, GraphQL: &GraphQL{}},
		},
		{
			name: "mcp tools",
			in:   Route{Hosts: []string{"x.test"}, MCP: &MCP{Tools: []string{"get_*", "search_code", "a.b/c-d"}}},
			want: Route{Hosts: []string{"x.test"}, MCP: &MCP{Tools: []string{"a.b/c-d", "get_*", "search_code"}}},
		},
		{name: "no hosts", in: Route{}, wantErr: "must name a host"},
		{name: "empty host", in: Route{Hosts: []string{" "}}, wantErr: "must name a host"},
		{name: "wildcard inside", in: Route{Hosts: []string{"api.*.com"}}, wantErr: "may hold *"},
		{name: "wildcard twice", in: Route{Hosts: []string{"*.*.com"}}, wantErr: "may hold *"},
		{name: "wildcard suffix", in: Route{Hosts: []string{"github.*"}}, wantErr: "may hold *"},
		{name: "bare *.", in: Route{Hosts: []string{"*."}}, wantErr: "must name a host"},
		{name: "host with path", in: Route{Hosts: []string{"github.com/x"}}, wantErr: "not a host name"},
		{name: "host with comma", in: Route{Hosts: []string{"a.com,b.com"}}, wantErr: "not a host name"},
		{name: "empty label", in: Route{Hosts: []string{"a..com"}}, wantErr: "empty label"},
		{name: "method", in: Route{Hosts: []string{"x.test"}, Methods: []string{"GE T"}}, wantErr: "not an HTTP method"},
		{name: "relative path", in: Route{Hosts: []string{"x.test"}, Paths: []string{"mcp"}}, wantErr: "must start with /"},
		{name: "query in path", in: Route{Hosts: []string{"x.test"}, Paths: []string{"/mcp?x=1"}}, wantErr: "plain path"},
		{name: "= in path", in: Route{Hosts: []string{"x.test"}, Paths: []string{"/a=b"}}, wantErr: "plain path"},
		{name: "path separator in path", in: Route{Hosts: []string{"x.test"}, Paths: []string{"/a,/b"}}, wantErr: "plain path"},
		{name: "empty segment", in: Route{Hosts: []string{"x.test"}, Paths: []string{"/a//b"}}, wantErr: "empty segment"},
		{name: "dot segment", in: Route{Hosts: []string{"x.test"}, Paths: []string{"/a/../b"}}, wantErr: `".." segment`},
		{name: "** not last", in: Route{Hosts: []string{"x.test"}, Paths: []string{"/a/**/b"}}, wantErr: "** before"},
		{name: "* inside a segment", in: Route{Hosts: []string{"x.test"}, Paths: []string{"/a/b*"}}, wantErr: "inside a segment"},
		{name: "graphql operation", in: Route{Hosts: []string{"x.test"}, GraphQL: &GraphQL{Operations: []string{"fragment"}}}, wantErr: "query, mutation or subscription"},
		{name: "graphql field", in: Route{Hosts: []string{"x.test"}, GraphQL: &GraphQL{Fields: []string{"a*b"}}}, wantErr: "GraphQL field"},
		{name: "mcp tool", in: Route{Hosts: []string{"x.test"}, MCP: &MCP{Tools: []string{"a b"}}}, wantErr: "MCP tool"},
		{name: "both protocols", in: Route{Hosts: []string{"x.test"}, GraphQL: &GraphQL{}, MCP: &MCP{}}, wantErr: "not both"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Normalize(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Normalize(%+v) error = %v, want one containing %q", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Normalize(%+v): %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Normalize(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseAndFormat(t *testing.T) {
	for _, tc := range []struct {
		text      string
		want      Route
		canonical string // Format(want), when it differs from text
	}{
		{text: "*", want: Route{Hosts: []string{"*"}}},
		{text: "api.github.com", want: Route{Hosts: []string{"api.github.com"}}},
		{text: "api.github.com/x=blocked", want: Route{Hosts: []string{"api.github.com"}, Paths: []string{"/x"}, Blocked: true}},
		{text: " github.com = blocked ", want: Route{Hosts: []string{"github.com"}, Blocked: true}, canonical: "github.com=blocked"},
		{text: "x.test/a,/", want: Route{Hosts: []string{"x.test"}, Paths: []string{"/", "/a"}}, canonical: "x.test/,/a"},
		{text: "*/v1", want: Route{Hosts: []string{"*"}, Paths: []string{"/v1"}}},
		{text: "GitHub.com,*.github.com", want: Route{Hosts: []string{"*.github.com", "github.com"}}, canonical: "*.github.com,github.com"},
		{
			text: "github.com/discobox-ai/discobox.git/**,/discobox-ai/discobox/**",
			want: Route{Hosts: []string{"github.com"}, Paths: []string{"/discobox-ai/discobox.git/**", "/discobox-ai/discobox/**"}},
		},
		{text: "GET api.github.com/repos/discobox-ai/discobox/**", want: Route{Hosts: []string{"api.github.com"}, Methods: []string{"GET"}, Paths: []string{"/repos/discobox-ai/discobox/**"}}},
		{text: "HEAD,GET api.github.com", want: Route{Hosts: []string{"api.github.com"}, Methods: []string{"GET", "HEAD"}}, canonical: "GET,HEAD api.github.com"},
		{text: "api.github.com/graphql graphql=query", want: Route{Hosts: []string{"api.github.com"}, Paths: []string{"/graphql"}, GraphQL: &GraphQL{Operations: []string{"query"}}}},
		{
			text:      "api.github.com/graphql graphql=mutation:createPullRequest,addComment",
			want:      Route{Hosts: []string{"api.github.com"}, Paths: []string{"/graphql"}, GraphQL: &GraphQL{Operations: []string{"mutation"}, Fields: []string{"addComment", "createPullRequest"}}},
			canonical: "api.github.com/graphql graphql=mutation:addComment,createPullRequest",
		},
		{text: "x.test graphql=*", want: Route{Hosts: []string{"x.test"}, GraphQL: &GraphQL{}}},
		{text: "x.test graphql=*:viewer", want: Route{Hosts: []string{"x.test"}, GraphQL: &GraphQL{Fields: []string{"viewer"}}}},
		{text: "x.test graphql=query,mutation,*", want: Route{Hosts: []string{"x.test"}, GraphQL: &GraphQL{}}, canonical: "x.test graphql=*"},
		{text: "x.test graphql=mutation:delete*=blocked", want: Route{Hosts: []string{"x.test"}, GraphQL: &GraphQL{Operations: []string{"mutation"}, Fields: []string{"delete*"}}, Blocked: true}},
		{text: "api.githubcopilot.com/mcp mcp=get_*,search_code", want: Route{Hosts: []string{"api.githubcopilot.com"}, Paths: []string{"/mcp"}, MCP: &MCP{Tools: []string{"get_*", "search_code"}}}},
		{text: "x.test mcp=*", want: Route{Hosts: []string{"x.test"}, MCP: &MCP{}}},
		{text: "x.test mcp=*=blocked", want: Route{Hosts: []string{"x.test"}, MCP: &MCP{}, Blocked: true}},
		// A tool named "blocked" reads as written, and a blocked route
		// calling it carries the suffix after it.
		{text: "x.test mcp=blocked", want: Route{Hosts: []string{"x.test"}, MCP: &MCP{Tools: []string{"blocked"}}}},
		{text: "x.test mcp=blocked=blocked", want: Route{Hosts: []string{"x.test"}, MCP: &MCP{Tools: []string{"blocked"}}, Blocked: true}},
		{text: "x.test graphql=query:blocked", want: Route{Hosts: []string{"x.test"}, GraphQL: &GraphQL{Operations: []string{"query"}, Fields: []string{"blocked"}}}},
		{text: "POST x.test/a/*/b mcp=run=blocked", want: Route{Hosts: []string{"x.test"}, Methods: []string{"POST"}, Paths: []string{"/a/*/b"}, MCP: &MCP{Tools: []string{"run"}}, Blocked: true}},
	} {
		got, err := Parse(tc.text)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.text, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("Parse(%q) = %+v, want %+v", tc.text, got, tc.want)
		}
		canonical := tc.canonical
		if canonical == "" {
			canonical = tc.text
		}
		if formatted := Format(got); formatted != canonical {
			t.Fatalf("Format(Parse(%q)) = %q, want %q", tc.text, formatted, canonical)
		}
		if again, err := Parse(Format(got)); err != nil || !reflect.DeepEqual(again, got) {
			t.Fatalf("Format did not round-trip %q: %+v, %v", tc.text, again, err)
		}
	}
}

func TestParseRefuses(t *testing.T) {
	for _, tc := range []struct {
		text, wantErr string
	}{
		{"", "must name a host"},
		{"=blocked", "must name a host"},
		// What a route once carried after its "=" is refused, not read as
		// blocked or dropped: either would change what the person meant.
		{"github.com=read-only queries", "no longer carries a use"},
		{"github.com=read-only", "no longer carries a use"},
		{"*.github.com/a*", "inside a segment"},
		{"api.*.com", "may hold *"},
		{"get api.github.com", "methods are upper case: GET"},
		{"x.test foo=bar", "not part of a route"},
		{"x.test graphql", "graphql=OPS"},
		{"x.test graphql=", "graphql=OPS"},
		{"x.test graphql=query:", "graphql=OPS:FIELDS"},
		{"x.test graphql=read", "query, mutation or subscription"},
		{"x.test graphql=query graphql=mutation", "one graphql="},
		{"x.test mcp", "mcp=TOOLS"},
		{"x.test mcp=a,,b", "mcp=TOOLS"},
		{"x.test mcp=a mcp=b", "one mcp="},
		{"x.test mcp=a graphql=query", "not both"},
		{"x.test/a,//b", "empty segment"},
		{"GET,,POST x.test", "not an HTTP method"},
		{"evil\x1b]0;x\x07.com", "not a host name"},
		{"github.com/a\x1b[2Jb", "plain path"},
		{"github.com/\u009b31m", "plain path"},
	} {
		_, err := Parse(tc.text)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("Parse(%q) error = %v, want one containing %q", tc.text, err, tc.wantErr)
		}
	}
}

// A lone word is the hosts, unless it is an HTTP method, which a person
// typed where a host belongs.
func TestALoneWordIsHosts(t *testing.T) {
	for _, host := range []string{"localhost", "intranet", "GITBOX"} {
		if got, err := Parse(host); err != nil || len(got.Hosts) != 1 || got.Hosts[0] != strings.ToLower(host) {
			t.Fatalf("Parse(%q) = %+v, %v", host, got, err)
		}
	}
	for text, want := range map[string]string{
		"GET":          "GET is a method; a route needs a host: `GET api.example.com/...`",
		"delete":       "DELETE is a method; a route needs a host: `DELETE api.example.com/...`",
		"Options":      "OPTIONS is a method",
		"POST=blocked": "POST is a method",
		"GET,POST":     "GET,POST are methods; a route needs a host: `GET,POST api.example.com/...`",
		"get,Head":     "GET,HEAD are methods",
	} {
		if _, err := Parse(text); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Parse(%q) error = %v, want one containing %q", text, err, want)
		}
	}
}

// Methods are upper case; a list of method names in any other case is told
// so rather than read as hosts.
func TestMethodsAreUpperCase(t *testing.T) {
	for text, want := range map[string]string{
		"POST,get api.github.com": "methods are upper case: POST,GET",
		"get api.github.com":      "methods are upper case: GET",
		"Put,Patch x.test/a":      "methods are upper case: PUT,PATCH",
	} {
		if _, err := Parse(text); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Parse(%q) error = %v, want one containing %q", text, err, want)
		}
	}
}

// A Parse error names the text it refused once.
func TestParseErrorsNameTheTextOnce(t *testing.T) {
	for _, text := range []string{"api.*.com=blocked", "x.test foo=bar", "x.test mcp=a graphql=query", "get api.github.com", "GET"} {
		_, err := Parse(text)
		if err == nil {
			t.Fatalf("Parse(%q) accepted it", text)
		}
		prefix := fmt.Sprintf("route %q: ", text)
		if !strings.HasPrefix(err.Error(), prefix) || strings.Count(err.Error(), fmt.Sprintf("%q", text)) != 1 {
			t.Fatalf("Parse(%q) error = %q, want it to start %q and name the text once", text, err, prefix)
		}
	}
}

func TestFromLegacy(t *testing.T) {
	for _, tc := range []struct {
		host, path string
		blocked    bool
		want       Route
		wantErr    bool
	}{
		{host: "GitHub.com", want: Route{Hosts: []string{"*.github.com", "github.com"}}},
		{host: "*", want: Route{Hosts: []string{"*"}}},
		{host: "*", path: "/v1", want: Route{Hosts: []string{"*"}, Paths: []string{"/v1/**"}}},
		{host: "githubcopilot.com", path: "/mcp/", blocked: true, want: Route{Hosts: []string{"*.githubcopilot.com", "githubcopilot.com"}, Paths: []string{"/mcp/**"}, Blocked: true}},
		{host: "x.test", path: "/", want: Route{Hosts: []string{"*.x.test", "x.test"}}},
		{host: "x.test", path: "/a/*", wantErr: true},
		{host: "*.x.test", wantErr: true},
		{host: "", wantErr: true},
	} {
		got, err := FromLegacy(tc.host, tc.path, tc.blocked)
		if (err != nil) != tc.wantErr {
			t.Fatalf("FromLegacy(%q, %q) error = %v, want error %v", tc.host, tc.path, err, tc.wantErr)
		}
		if err == nil && !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("FromLegacy(%q, %q) = %+v, want %+v", tc.host, tc.path, got, tc.want)
		}
	}
}
