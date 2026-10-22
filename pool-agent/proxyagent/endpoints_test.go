package proxyagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/judge"
	"github.com/discobox-ai/discobox/proxy"
)

// sandboxCreate is a discobox API create as the discobox CLI sends one through
// the gate: the prompt first and long, the grants last.
func sandboxCreate(body string) proxy.SecretAuthorizeRequest {
	req := withBody([]byte(body), "application/json")
	req.Host = GateHost()
	req.URL = "https://" + GateHost() + "/projects/default/sandboxes"
	return req
}

func describedMetadata(t *testing.T, req proxy.SecretAuthorizeRequest) (*judge.Request, map[string]any) {
	t.Helper()
	evidence := evidenceOf(context.Background(), req)
	if evidence.Body == nil || len(evidence.Body.Metadata) == 0 {
		t.Fatalf("body = %+v, want it described with metadata", evidence.Body)
	}
	var metadata map[string]any
	if err := json.Unmarshal(evidence.Body.Metadata, &metadata); err != nil {
		t.Fatalf("metadata %s: %v", evidence.Body.Metadata, err)
	}
	return evidence, metadata
}

// A create is described by what it hands the new discobox, from the first ask:
// the grants a use of the discobox credential is approved around sit at the
// end of a body whose prompt can be kilobytes, and the keys alone say only
// that there are some.
func TestADiscoboxCreateIsDescribedByWhatItGrants(t *testing.T) {
	prompt := "Implement discobox-ai/discobox issue #43. " + strings.Repeat("Read the ADR first. ", 200) + testEphemeral
	body := fmt.Sprintf(`{
		"config": {
			"name": "worker",
			"prompt": [%q],
			"source": {"kind": "git", "localDirectory": "/home/u/src/app",
				"checkout": {"commit": "c89f6d7e", "refName": "main", "refType": "branch"}},
			"sourceCodeReferences": {"/home/u/src/x": {"slug": "x", "kind": "git", "checkout": {"commit": "f8b7"}}},
			"secrets": [{"env": "NPM_TOKEN", "value": "npm_itsownsecret"}],
			"env": {"STRIPE_KEY": "sk_live_plain", "DEBUG": "1"}
		},
		"origin": {"host": "laptop"},
		"grants": [{"wellKnownId": "com.github.api", "grantTTLSeconds": 3600, "uses": [
			{"description": "Push the branch issue-43 to github.com/discobox-ai/discobox, never main"},
			{"description": "Open one pull request from issue-43 into main"}
		]}]
	}`, prompt)

	evidence, metadata := describedMetadata(t, sandboxCreate(body))
	if evidence.Endpoint == nil || evidence.Endpoint.Name != judge.EndpointDiscoboxSandboxCreate {
		t.Fatalf("endpoint = %+v, want the discobox API's create", evidence.Endpoint)
	}
	if evidence.OperationInBody() == "" {
		t.Fatal("a create's operation is its body, and an allow for one must not stand for the route")
	}
	grants, _ := metadata["grants"].([]any)
	if len(grants) != 1 || metadata["grantsTotal"] != float64(1) {
		t.Fatalf("grants = %v, want the one grant", metadata["grants"])
	}
	grant, _ := grants[0].(map[string]any)
	uses, _ := grant["uses"].([]any)
	if grant["credential"] != "com.github.api" || grant["ttlSeconds"] != float64(3600) || len(uses) != 2 ||
		uses[0] != "Push the branch issue-43 to github.com/discobox-ai/discobox, never main" {
		t.Fatalf("grant = %v, want the credential, its lifetime, and each use as it was granted", grant)
	}
	said, _ := metadata["prompt"].(string)
	if !strings.HasPrefix(said, "Implement discobox-ai/discobox issue #43.") || strings.Contains(said, testEphemeral) {
		t.Fatalf("prompt = %q, want its start, with no sentinel in it", said)
	}
	if metadata["promptBytes"] != float64(len(prompt)) {
		t.Fatalf("promptBytes = %v, want %d", metadata["promptBytes"], len(prompt))
	}
	source, _ := metadata["source"].(map[string]any)
	if source["localDirectory"] != "/home/u/src/app" || source["commit"] != "c89f6d7e" || source["refName"] != "main" {
		t.Fatalf("source = %v, want where it starts from", metadata["source"])
	}
	if others, _ := metadata["otherSources"].([]any); len(others) != 1 {
		t.Fatalf("otherSources = %v, want the one other source", metadata["otherSources"])
	}
	secrets, _ := metadata["secrets"].([]any)
	secret, _ := secrets[0].(map[string]any)
	if secret["env"] != "NPM_TOKEN" || secret["value"] != "inline, not shown" {
		t.Fatalf("secrets = %v, want the variable and that the value is inline", metadata["secrets"])
	}
	if env, _ := metadata["env"].([]any); len(env) != 2 || env[0] != "DEBUG" || env[1] != "STRIPE_KEY" {
		t.Fatalf("env = %v, want the variables it sets named", metadata["env"])
	}
	if data := string(evidence.Body.Metadata); strings.Contains(data, "npm_itsownsecret") || strings.Contains(data, "sk_live_plain") || strings.Contains(data, "laptop") {
		t.Fatalf("metadata %s shows what is not the operation", data)
	}
}

// A create with no source is said to have none, rather than left to read as
// one whose source went unmentioned.
func TestADiscoboxCreateWithNoSourceSaysSo(t *testing.T) {
	_, metadata := describedMetadata(t, sandboxCreate(`{"config": {"name": "empty"}}`))
	if metadata["source"] != "none" {
		t.Fatalf("source = %v, want none", metadata["source"])
	}
}

// A create that grants nothing says so. Its prompt is clipped with "…", and
// without a count of none the judge read that as grants left out and refused.
func TestADiscoboxCreateGrantingNothingCountsNone(t *testing.T) {
	prompt := "Implement discobox-ai/discobox issue #43, a Wave 1 sub-issue of epic #42. " + strings.Repeat("Follow CLAUDE.md. ", 20)
	_, metadata := describedMetadata(t, sandboxCreate(fmt.Sprintf(`{"config": {"name": "w", "prompt": [%q]}}`, prompt)))
	if metadata["grantsTotal"] != float64(0) || metadata["grants"] != nil {
		t.Fatalf("grantsTotal = %v, grants = %v; want a count of none and no list", metadata["grantsTotal"], metadata["grants"])
	}
	if said, _ := metadata["prompt"].(string); !strings.HasSuffix(said, "…") {
		t.Fatalf("prompt = %q, want it clipped", said)
	}
}

// Grants that cannot be read are not counted: a count of none would say the
// body grants nothing when it may grant anything.
func TestADiscoboxCreateWithUnreadableGrantsIsNotCounted(t *testing.T) {
	_, metadata := describedMetadata(t, sandboxCreate(`{"config": {"name": "w"}, "grants": {"wellKnownId": "com.github.api"}}`))
	if _, counted := metadata["grantsTotal"]; counted {
		t.Fatalf("grantsTotal = %v, want none said for grants that could not be read", metadata["grantsTotal"])
	}
}

// Many grants are more than metadata may say: the first are listed and the
// total counted, so the judge knows to ask for the rest.
func TestADiscoboxCreateGrantingMoreThanFitsIsCounted(t *testing.T) {
	var grants []string
	for i := range 40 {
		grants = append(grants, fmt.Sprintf(`{"secretId": "secret-%d", "envVar": "TOKEN_%d", "uses": [{"description": "%s"}]}`,
			i, i, strings.Repeat("use it for something specific ", 5)))
	}
	evidence, metadata := describedMetadata(t, sandboxCreate(`{"config": {"name": "w"}, "grants": [`+strings.Join(grants, ",")+`]}`))
	if len(evidence.Body.Metadata) > judge.MaxMetadataBytes {
		t.Fatalf("metadata is %d bytes, want at most %d", len(evidence.Body.Metadata), judge.MaxMetadataBytes)
	}
	listed, _ := metadata["grants"].([]any)
	if metadata["grantsTotal"] != float64(40) || len(listed) == 0 || len(listed) >= 40 {
		t.Fatalf("grants listed %d, total %v; want some of them and a count of all 40", len(listed), metadata["grantsTotal"])
	}
}

// The endpoint is the create and nothing else on the API: listing discoboxes
// is not making one.
func TestOnlyADiscoboxCreateIsRecognizedAsOne(t *testing.T) {
	req := sandboxCreate(`{}`)
	if recognize(req).endpoint == nil {
		t.Fatal("the create was not recognized")
	}
	req.Method = http.MethodGet
	if recognize(req).endpoint != nil {
		t.Fatal("listing discoboxes was recognized as making one")
	}
	req.Method, req.Host = http.MethodPost, "api.example.com"
	if recognize(req).endpoint != nil {
		t.Fatal("another host's path was recognized as the discobox API's")
	}
}

// apiCall is a call to the discobox API through the gate, at path.
func apiCall(method, path, body string) proxy.SecretAuthorizeRequest {
	req := withBody([]byte(body), "application/json")
	if body == "" {
		req = authorizeRequest()
		req.Header.Del("Content-Type")
		req.Header.Del("Content-Length")
	}
	req.Method = method
	req.Host = GateHost()
	req.URL = "https://" + GateHost() + path
	return req
}

// What `discobox new` sends after a create to finish making the discobox is
// each named, so the judge is told it is that and not something else under
// the same use. The reads carry nothing in their bodies and may stand, so a
// poll once a second is not a judge once a second; the push and the report
// are read every time.
func TestTheCallsThatFinishADiscoboxCreateAreRecognized(t *testing.T) {
	const sandbox = "/projects/default/sandboxes/sbx_99sxwvjtnv0sgsdw"
	for _, tc := range []struct {
		name, method, path, body string
		endpoint                 string
		protocol                 string
		stands                   bool
	}{
		{name: "poll", method: http.MethodGet, path: sandbox,
			endpoint: judge.EndpointDiscoboxSandboxGet, stands: true},
		{name: "discovery", method: http.MethodGet, path: sandbox + "/git-origins/primary.git/info/refs?service=git-receive-pack",
			endpoint: judge.EndpointDiscoboxSandboxOriginRefs, stands: true},
		{name: "push", method: http.MethodPost, path: sandbox + "/git-origins/primary.git/git-receive-pack",
			endpoint: judge.EndpointDiscoboxSandboxOriginPush, protocol: judge.ProtocolGitReceivePack},
		{name: "report", method: http.MethodPost, path: sandbox + "/complete-source-push", body: `{"sources": {"primary": "f3d4e53c"}}`,
			endpoint: judge.EndpointDiscoboxSandboxSourcePushed},
	} {
		evidence := evidenceOf(context.Background(), apiCall(tc.method, tc.path, tc.body))
		if evidence.Endpoint == nil || evidence.Endpoint.Name != tc.endpoint {
			t.Fatalf("%s: endpoint = %+v, want %s", tc.name, evidence.Endpoint, tc.endpoint)
		}
		if tc.protocol != "" && (evidence.Protocol == nil || evidence.Protocol.Name != tc.protocol) {
			t.Fatalf("%s: protocol = %+v, want %s", tc.name, evidence.Protocol, tc.protocol)
		}
		if stands := evidence.OperationInBody() == ""; stands != tc.stands {
			t.Fatalf("%s: may stand = %v, want %v", tc.name, stands, tc.stands)
		}
		if len(judge.GuidanceFor(evidence)) == 0 {
			t.Fatalf("%s: no guidance says what it is", tc.name)
		}
	}
}

// The report is described by what it says was pushed: the commit each
// source of the discobox will start from.
func TestASourcePushedReportIsDescribedByItsCommits(t *testing.T) {
	_, metadata := describedMetadata(t, apiCall(http.MethodPost,
		"/projects/default/sandboxes/sbx_1/complete-source-push", `{"sources": {"primary": "f3d4e53c", "hooks": "a2b5db14"}}`))
	sources, _ := metadata["sources"].(map[string]any)
	if sources["primary"] != "f3d4e53c" || sources["hooks"] != "a2b5db14" {
		t.Fatalf("sources = %v, want each source's commit", metadata["sources"])
	}
}

// Reading one discobox is not listing them, and the discobox API's paths
// mean nothing on another host.
func TestOnlyReadingOneDiscoboxIsRecognizedAsIt(t *testing.T) {
	if got := recognize(apiCall(http.MethodGet, "/projects/default/sandboxes", "")).endpoint; got != nil {
		t.Fatalf("listing discoboxes was recognized as %s", got.Name)
	}
	req := apiCall(http.MethodGet, "/projects/default/sandboxes/sbx_1", "")
	req.Host = "api.example.com"
	if recognize(req).endpoint != nil {
		t.Fatal("another host's path was recognized as the discobox API's")
	}
}
