package cli

import (
	"testing"

	"github.com/spf13/pflag"

	apimodel "github.com/discobox-ai/discobox/api/model"
)

func TestSecretRoutesFlagsReplaceOrClear(t *testing.T) {
	newFlags := func(args ...string) *pflag.FlagSet {
		flags := pflag.NewFlagSet("update", pflag.ContinueOnError)
		flags.StringArray("route", nil, "")
		flags.Bool("no-routes", false, "")
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
		"--route", "githubcopilot.com/mcp/=read-only queries = nothing else",
		"--route", "github.com= reading a pull request "))
	if !ok || err != nil || len(routes) != 3 {
		t.Fatalf("routes = %#v, ok = %t, err = %v, want all three", routes, ok, err)
	}
	describe := func(route apimodel.SecretRouteInput) [3]string {
		return [3]string{route.Host, route.Path.Or(""), route.Use.Or("")}
	}
	for i, want := range [][3]string{
		{"githubcopilot.com", "", ""},
		{"githubcopilot.com", "/mcp", "read-only queries = nothing else"},
		{"github.com", "", "reading a pull request"},
	} {
		if got := describe(routes[i]); got != want {
			t.Fatalf("route %d = %q, want %q", i, got, want)
		}
	}
	if routes, ok, err := secretRoutesFlags(newFlags("--no-routes")); !ok || err != nil || routes == nil || len(routes) != 0 {
		t.Fatalf("routes = %#v, ok = %t, err = %v, want an empty list that clears", routes, ok, err)
	}
	if _, _, err := secretRoutesFlags(newFlags("--no-routes", "--route", "github.com")); err == nil {
		t.Fatal("--no-routes with --route succeeded, want them refused together")
	}
	for _, bad := range []string{"=a use", "/mcp", "github.com/a//b", "github.com/a b"} {
		if _, _, err := secretRoutesFlags(newFlags("--route", bad)); err == nil {
			t.Fatalf("--route %q succeeded, want it refused", bad)
		}
	}
}
