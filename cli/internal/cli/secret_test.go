package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	apimodel "github.com/discobox-ai/discobox/api/model"
)

func TestSecretRoutesFlagsReplaceOrClear(t *testing.T) {
	newFlags := func(args ...string) *pflag.FlagSet {
		flags := pflag.NewFlagSet("update", pflag.ContinueOnError)
		flags.StringArray("route", nil, "")
		flags.Bool("clear-route", false, "")
		if err := flags.Parse(args); err != nil {
			t.Fatal(err)
		}
		return flags
	}
	if _, ok, err := secretRoutesFlags(newFlags()); ok || err != nil {
		t.Fatalf("ok = %t, err = %v, want the routes left alone", ok, err)
	}
	routes, ok, err := secretRoutesFlags(newFlags(
		"--route", "GitHubCopilot.com",
		"--route", "githubcopilot.com/mcp/=blocked",
		"--route", "github.com = blocked "))
	if !ok || err != nil || len(routes) != 3 {
		t.Fatalf("routes = %#v, ok = %t, err = %v, want all three", routes, ok, err)
	}
	type described struct {
		host, path string
		blocked    bool
	}
	describe := func(route apimodel.SecretRouteInput) described {
		return described{route.Host, route.Path.Or(""), route.Blocked.Or(false)}
	}
	for i, want := range []described{
		{"githubcopilot.com", "", false},
		{"githubcopilot.com", "/mcp", true},
		{"github.com", "", true},
	} {
		if got := describe(routes[i]); got != want {
			t.Fatalf("route %d = %+v, want %+v", i, got, want)
		}
	}
	if routes, ok, err := secretRoutesFlags(newFlags("--clear-route")); !ok || err != nil || routes == nil || len(routes) != 0 {
		t.Fatalf("routes = %#v, ok = %t, err = %v, want an empty list that clears", routes, ok, err)
	}
	if _, ok, err := secretRoutesFlags(newFlags("--clear-route=false")); ok || err != nil {
		t.Fatalf("--clear-route=false: ok = %t, err = %v, want the routes left alone", ok, err)
	}
	// Given beside the flag it clears, the command refuses both.
	refused := runCLIRefused(t, "http://127.0.0.1:1", "secret", "update", "github", "--clear-route", "--route", "github.com")
	if !strings.Contains(refused, "--clear-route and --route cannot be used together") {
		t.Fatalf("--clear-route with --route = %q, want them refused together", refused)
	}
	// Refused by value: --clear-route=false asks for nothing, so --route beside
	// it gets as far as the server (none here).
	if refused := runCLIRefused(t, "http://127.0.0.1:1", "secret", "update", "github", "--clear-route=false", "--route", "github.com"); strings.Contains(refused, "cannot be used together") {
		t.Fatalf("--clear-route=false with --route = %q, want it let through", refused)
	}
	for _, bad := range []string{"=blocked", "/mcp", "github.com/a//b", "github.com/a b", "github.com=read-only queries"} {
		if _, _, err := secretRoutesFlags(newFlags("--route", bad)); err == nil {
			t.Fatalf("--route %q succeeded, want it refused", bad)
		}
	}
}

// runCLIRefused runs the CLI against serverURL and returns why it refused,
// failing when it did not.
func runCLIRefused(t *testing.T, serverURL string, args ...string) string {
	t.Helper()
	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"--server", serverURL, "--project", "project-1"}, args...))
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("%v was not refused", args)
	}
	return err.Error()
}
