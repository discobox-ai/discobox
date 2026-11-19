package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/api/openapi"
	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/server/internal/handlers"
	"github.com/discobox-ai/discobox/server/internal/services"
)

func TestEveryPublishedOperationIsServedByTheServer(t *testing.T) {
	type operation struct {
		ID     string `yaml:"operationId"`
		Shared bool   `yaml:"x-sandbox-agent"`
	}
	readPaths := func(data []byte) map[string]map[string]operation {
		t.Helper()
		var doc struct {
			Paths map[string]map[string]operation `yaml:"paths"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Paths
	}

	controlPlane, err := serverapi.NewServer(handlers.New(services.Services{}))
	if err != nil {
		t.Fatal(err)
	}
	agent, err := sandboxapi.NewServer(nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy := chi.NewRouter()
	registerSandboxAgentTerminalRoutes(proxy, nil)

	parameters := regexp.MustCompile(`\{[^}]+\}`)
	proxyStubs := sandboxProxyStubs(t)
	serverPaths := readPaths(openapi.ServerYAML)
	sandboxPaths := readPaths(openapi.SandboxYAML)
	for path, methods := range serverPaths {
		for verb, op := range methods {
			method := strings.ToUpper(verb)
			sample := parameters.ReplaceAllString(path, "example")
			route, ok := controlPlane.FindRoute(method, sample)
			if !ok || route.OperationID() != op.ID {
				t.Errorf("%s %s: missing generated route for %s", method, path, op.ID)
			}
			forwarded := proxy.Match(chi.NewRouteContext(), method, sample)
			if forwarded != proxyStubs[route.Name()] {
				t.Errorf("%s %s: proxy registered = %v, generated handler is a proxy stub = %v", method, path, forwarded, proxyStubs[route.Name()])
			}
			if forwarded && !op.Shared {
				t.Errorf("%s %s: forwarded operation %s is missing x-sandbox-agent", method, path, op.ID)
			}
			agentPath := "/api" + strings.Replace(path, "/sandboxes/{sandboxId}/agent/", "/sandboxes/{sandboxId}/", 1)
			if op.Shared && sandboxPaths[agentPath][verb].ID != op.ID {
				t.Errorf("%s %s: agent contract does not publish shared operation %s", method, path, op.ID)
			}
		}
	}

	// These operations are used by the pool on the sandbox agent's listener.
	// Keep their transport paths and methods even when the server routes move.
	private := map[string]string{
		"GET status":                       "get-sandbox-agent-status",
		"GET status/wait":                  "wait-sandbox-agent-status",
		"GET inventory":                    "get-sandbox-agent-inventory",
		"GET runtime-config":               "get-sandbox-runtime-config",
		"PUT runtime-config":               "put-sandbox-runtime-config",
		"POST autostop/confirm":            "confirm-sandbox-autostop",
		"POST autostop/resumed":            "resume-sandbox-autostop",
		"POST cache/clear":                 "clear-sandbox-cache",
		"GET sources":                      "get-sandbox-source-states",
		"GET sources/{slug}/project-layer": "get-sandbox-source-project-layer",
		"PATCH meta":                       "update-sandbox-agent-meta",
		"POST harness-config-capture":      "capture-sandbox-harness-config",
		"POST harness-config-ack":          "acknowledge-sandbox-harness-config",
		"POST harness-config-reconsider":   "reconsider-sandbox-harness-config",
	}
	for key, id := range private {
		method, suffix, _ := strings.Cut(key, " ")
		path := "/api/projects/{projectId}/sandboxes/{sandboxId}/" + suffix
		sample := parameters.ReplaceAllString(path, "example")
		route, ok := agent.FindRoute(method, sample)
		if !ok || route.OperationID() != id {
			t.Errorf("%s %s: missing generated agent route for %s", method, path, id)
		}
		if _, ok := controlPlane.FindRoute(method, sample); ok || proxy.Match(chi.NewRouteContext(), method, sample) || legacyServerPathMatcher.Match(chi.NewRouteContext(), http.MethodGet, sample) {
			t.Errorf("%s %s: agent-only operation is available on the server", method, path)
		}
	}
}

// Identify placeholder implementations without dispatching requests that
// need real services or operation-specific request bodies.
func sandboxProxyStubs(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("../handlers/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("locate server handlers: files=%v, error=%v", files, err)
	}
	stubs := make(map[string]bool)
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			method, ok := declaration.(*ast.FuncDecl)
			if !ok || method.Recv == nil {
				continue
			}
			ast.Inspect(method.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if ok {
					if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "sandboxAgentRuntimeNotImplemented" {
						stubs[method.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	return stubs
}
