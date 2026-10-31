package credentialpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func policyFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

const repoPolicy = `{"version":1,"parameters":{"owner":{"type":"pathSegment"},"repo":{"type":"pathSegment"},"suffix":{"type":"pathSegment","optional":true,"default":""}},"routes":[{"hosts":["API.GITHUB.COM"],"methods":["post"],"paths":["/repos/${owner}/${repo}/issues${suffix}"]}]}`

func TestIndependentPolicyArgumentsAndSnapshot(t *testing.T) {
	path := policyFile(t, repoPolicy)
	var refs References
	for _, repo := range []string{"first", "second"} {
		if err := refs.Add(path); err != nil {
			t.Fatal(err)
		}
		for _, arg := range []string{"owner=acme", "repo=" + repo} {
			if err := refs.Argument(arg); err != nil {
				t.Fatal(err)
			}
		}
	}
	routes, err := refs.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 || routes[0].Paths[0] != "/repos/acme/first/issues" || routes[1].Paths[0] != "/repos/acme/second/issues" || routes[0].Hosts[0] != "api.github.com" || routes[0].Methods[0] != "POST" {
		t.Fatalf("routes=%+v", routes)
	}
	if err := os.WriteFile(path, []byte(`invalid`), 0600); err != nil {
		t.Fatal(err)
	}
	if routes[0].Paths[0] != "/repos/acme/first/issues" {
		t.Fatal("resolved snapshot changed")
	}
	if _, err := refs.Resolve(); err == nil {
		t.Fatal("reapplying invalid file should fail")
	}
}

func TestPolicyRejectsInvalidDocuments(t *testing.T) {
	valid := `{"version":1,"routes":[{"hosts":["example.com"]}]}`
	for name, body := range map[string]string{
		"version":                  strings.Replace(valid, `"version":1`, `"version":2`, 1),
		"missing version":          `{"routes":[{"hosts":["example.com"]}]}`,
		"missing routes":           `{"version":1}`,
		"empty routes":             `{"version":1,"routes":[]}`,
		"unknown":                  strings.Replace(valid, `"version":1`, `"version":1,"typo":true`, 1),
		"case folded key":          strings.Replace(valid, `"hosts"`, `"Hosts"`, 1),
		"duplicate":                strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		"nested duplicate":         strings.Replace(valid, `"hosts":["example.com"]`, `"hosts":["example.com"],"hosts":["evil.com"]`, 1),
		"trailing":                 valid + ` {}`,
		"malformed":                valid[:len(valid)-1],
		"null":                     strings.Replace(valid, `["example.com"]`, `null`, 1),
		"bad host":                 strings.Replace(valid, `example.com`, `bad host`, 1),
		"undeclared":               strings.Replace(valid, `"hosts":["example.com"]`, `"hosts":["example.com"],"paths":["/${repo}"]`, 1),
		"host placeholder":         strings.Replace(valid, `example.com`, `${repo}`, 1),
		"method placeholder":       strings.Replace(valid, `"hosts":["example.com"]`, `"hosts":["example.com"],"methods":["${method}"]`, 1),
		"protocol unknown":         strings.Replace(valid, `"hosts":["example.com"]`, `"hosts":["example.com"],"mcp":{"tool":[]}`, 1),
		"parameter type":           `{"version":1,"parameters":{"repo":{"type":"string"}},"routes":[{"hosts":["example.com"]}]}`,
		"optional without default": `{"version":1,"parameters":{"repo":{"type":"pathSegment","optional":true}},"routes":[{"hosts":["example.com"]}]}`,
		"required with default":    `{"version":1,"parameters":{"repo":{"type":"pathSegment","default":"foo"}},"routes":[{"hosts":["example.com"]}]}`,
		"invalid default":          `{"version":1,"parameters":{"repo":{"type":"pathSegment","optional":true,"default":"*"}},"routes":[{"hosts":["example.com"]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := policyFile(t, body)
			_, err := (References{{Path: path}}).Resolve()
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestPolicyArgumentsCannotBroadenPaths(t *testing.T) {
	path := policyFile(t, repoPolicy)
	for _, value := range []string{"*", "**", "../x", "..", ".", "a/b", `a\b`, "%2f", "%252f", "%2e%2e", "${owner}", "a?b", "a#b", "a=b", "a\n", ""} {
		t.Run(value, func(t *testing.T) {
			_, err := (References{{Path: path, Args: map[string]string{"owner": "acme", "repo": value}}}).Resolve()
			if err == nil {
				t.Fatal("unsafe argument accepted")
			}
		})
	}
	for name, args := range map[string]map[string]string{"missing": {"owner": "acme"}, "unknown": {"owner": "acme", "repo": "widget", "typo": "x"}} {
		t.Run(name, func(t *testing.T) {
			if _, err := (References{{Path: path, Args: args}}).Resolve(); err == nil {
				t.Fatal("invalid arguments accepted")
			}
		})
	}
}

func TestPolicyReferenceArgumentScope(t *testing.T) {
	var refs References
	if err := refs.Argument("owner=acme"); err == nil {
		t.Fatal("argument before policy accepted")
	}
	if err := refs.Add(""); err == nil {
		t.Fatal("empty file accepted")
	}
	if err := refs.Add("one.json"); err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"owner=acme"} {
		if err := refs.Argument(arg); err != nil {
			t.Fatal(err)
		}
	}
	for _, arg := range []string{"owner=again", "bad", "policy=x", "bad-name=x"} {
		if err := refs.Argument(arg); err == nil {
			t.Fatalf("argument %q accepted", arg)
		}
	}
}

func TestMergedPoliciesKeepBlockedAndProtocolRoutes(t *testing.T) {
	one := policyFile(t, `{"version":1,"routes":[{"hosts":["example.com"],"paths":["/**"]},{"hosts":["example.com"],"paths":["/private/**"],"blocked":true}]}`)
	two := policyFile(t, `{"version":1,"routes":[{"hosts":["graphql.example.com"],"graphql":{"operations":["query"],"fields":["issues"]}},{"hosts":["mcp.example.com"],"mcp":{"tools":["file_issue"]}}]}`)
	routes, err := (References{{Path: one}, {Path: two}}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 4 || !routes[1].Blocked || routes[2].GraphQL == nil || routes[3].MCP == nil {
		t.Fatalf("routes=%+v", routes)
	}
}

// A compact template can repeat a large default; the budget must be checked
// before appending each expansion, across all paths of a policy.
func TestPolicyBoundsExpandedPaths(t *testing.T) {
	for _, paths := range []string{
		`["/` + strings.Repeat("${part}", 20) + `"]`,
		`["/${part}","/${part}","/${part}","/${part}","/${part}"]`,
	} {
		body := `{"version":1,"parameters":{"part":{"type":"pathSegment","optional":true,"default":"` + strings.Repeat("a", policyByteLimit/4) + `"}},"routes":[{"hosts":["example.com"],"paths":` + paths + `}]}`
		_, err := (References{{Path: policyFile(t, body)}}).Resolve()
		if err == nil || !strings.Contains(err.Error(), "expanded paths exceed 1 MiB") {
			t.Fatalf("error=%v, want expanded output refusal", err)
		}
	}
}
