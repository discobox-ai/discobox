package console

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/bindingplan"

	"github.com/discobox-ai/discobox/pkg/secretroute"
)

func TestRouteTableBoundsCartesianOutputAndPreservesCompactRoutes(t *testing.T) {
	t.Parallel()
	hosts, paths := make([]string, 2000), make([]string, 2000)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("host%d.example.com", i)
		paths[i] = fmt.Sprintf("/path%d", i)
	}
	route := secretroute.Route{Hosts: hosts, Paths: paths}
	if routeTableFits([]secretroute.Route{route}) {
		t.Fatal("unbounded Cartesian product admitted")
	}
	sec := RouteTable("Routes", &RequestPart{Routes: []secretroute.Route{route}})
	if len(sec.Lines) != 2 || sec.Lines[1].Text != secretroute.Format(route) {
		t.Fatal("fallback changed route restrictions")
	}
	route = secretroute.Route{Hosts: hosts[:100], MCP: &secretroute.MCP{Tools: []string{strings.Repeat("tool", 10000)}}}
	if routeTableFits([]secretroute.Route{route}) {
		t.Fatal("repeated protocol bytes exceeded output bound")
	}
}

// replacedRows is what replacedFields says, a row to a line, its label
// before a bar so where the field starts shows.
func replacedRows(fields []DetailField) string {
	var rows []string
	for _, f := range fields {
		rows = append(rows, f.Label+"|"+f.Value)
	}
	return strings.Join(rows, "\n")
}

// The card says, of each binding the asking discobox holds of the secret,
// what approving a part does to it — kept, gone, losing its use, split — by
// the rule the server applies, and when a new one is made (ADR 26-11-02-022
// §5).
func TestTheCardSaysWhatEachBindingBecomes(t *testing.T) {
	t.Parallel()
	at := func(m int) time.Time { return time.Date(2026, 11, 2, 0, m, 0, 0, time.UTC) }
	standing := []SecretBinding{
		{ID: "old", Use: true, EnvVar: "GITHUB_TOKEN", Created: at(0)},
		{ID: "asked", Use: true, EnvVar: "GH_TOKEN", Created: at(1)},
		{ID: "both", Use: true, Delegate: true, EnvVar: "GH_LEAD", Created: at(2)},
	}
	use := replacedRows(ReplacedFields(standing, false, "GH_TOKEN", bindingplan.Replace, nil, nil))
	for _, want := range []string{
		"the one in GH_TOKEN, sent to the secret's routes — kept, takes this approval",
		"the one in GITHUB_TOKEN, sent to the secret's routes — goes",
		"the one in GH_LEAD, sent to the secret's routes — loses its use; its delegation stays",
	} {
		if !strings.Contains(use, want) {
			t.Errorf("use part:\n%s\nwant %q", use, want)
		}
	}
	delegate := replacedRows(ReplacedFields(standing, true, "GH_TOKEN", "", nil, nil))
	if !strings.Contains(delegate, "stays the delegation; its use moves to a binding of its own in GH_LEAD") || strings.Contains(delegate, "GITHUB_TOKEN") {
		t.Errorf("delegate part:\n%s", delegate)
	}
	if got := replacedRows(ReplacedFields(standing[2:], false, "GH_TOKEN", bindingplan.Replace, nil, nil)); !strings.Contains(got, "and a new binding is made") {
		t.Errorf("a use part with nothing to keep:\n%s", got)
	}
	// Both parts approved over the binding that uses and delegates: the use
	// part takes its use, and the delegate part then updates the delegation
	// left — no split, no second new use binding.
	both := standing[2:]
	if got := replacedRows(ReplacedFields(both, true, "GH_TOKEN", bindingplan.Replace, nil, nil)); !strings.Contains(got, "the delegation, sent to the secret's routes — kept, takes this approval") ||
		strings.Contains(got, "moves") || strings.Contains(got, "new binding") {
		t.Errorf("delegate part after the use part:\n%s", got)
	}
}

// A binding is set against the routes the part asks for as a diff: what it
// loses, and — on the binding kept, whatever its fate — what it gains, the
// routes both name counted rather than listed again. A binding that goes says
// how many of its routes the approval carries on, never that they are the
// same, and a side with no routes of its own is the secret's.
func TestTheCardDiffsAReplacedBindingsRoutes(t *testing.T) {
	t.Parallel()
	at := func(m int) time.Time { return time.Date(2026, 11, 2, 0, m, 0, 0, time.UTC) }
	route := func(path string) secretroute.Route {
		return secretroute.Route{Hosts: []string{"api.github.com"}, Paths: []string{path}}
	}
	routes := func(paths ...string) []secretroute.Route {
		var out []secretroute.Route
		for _, p := range paths {
			out = append(out, route(p))
		}
		return out
	}
	r := func(path string) string { return secretroute.Format(route(path)) }
	for _, tc := range []struct {
		name     string
		standing []SecretBinding
		delegate bool
		asked    []secretroute.Route
		secret   []secretroute.Route
		want     []string
	}{{
		name: "the kept one loses and gains, the one that goes only loses",
		standing: []SecretBinding{
			{ID: "old", Use: true, EnvVar: "GITHUB_TOKEN", Created: at(0), Routes: routes("/a", "/gone")},
			{ID: "asked", Use: true, EnvVar: "GH_TOKEN", Created: at(1), Routes: routes("/a", "/b", "/dropped")},
		},
		asked: routes("/a", "/b", "/new"),
		want: []string{
			"replaces|the one in GITHUB_TOKEN — goes",
			"|  1 route also in this approval",
			"|- " + r("/gone"),
			"|the one in GH_TOKEN — kept, takes this approval",
			"|  2 routes unchanged",
			"|- " + r("/dropped"),
			"|+ " + r("/new"),
		},
	}, {
		// The split binding stays the delegation and takes the approval's
		// routes in place, so a widening is said on it.
		name:     "a split delegation gains",
		standing: []SecretBinding{{ID: "both", Use: true, Delegate: true, EnvVar: "GH_TOKEN", Created: at(0), Routes: routes("/a")}},
		delegate: true,
		asked:    routes("/a", "/wider"),
		want: []string{
			"replaces|the one in GH_TOKEN — stays the delegation; its use moves to a binding of its own in GH_TOKEN",
			"|  1 route unchanged",
			"|+ " + r("/wider"),
		},
	}, {
		name: "one that goes, all of it carried on, is not the same",
		standing: []SecretBinding{
			{ID: "old", Use: true, EnvVar: "GITHUB_TOKEN", Created: at(0), Routes: routes("/a")},
			{ID: "asked", Use: true, EnvVar: "GH_TOKEN", Created: at(1), Routes: routes("/a", "/b")},
		},
		asked: routes("/a", "/b"),
		want: []string{
			"replaces|the one in GITHUB_TOKEN — goes",
			"|  every route also in this approval",
			"|the one in GH_TOKEN — kept, takes this approval",
			"|  the same routes",
		},
	}, {
		name:     "a part naming no routes is set against the secret's",
		standing: []SecretBinding{{ID: "asked", Use: true, EnvVar: "GH_TOKEN", Created: at(0), Routes: routes("/a", "/narrow")}},
		secret:   routes("/a", "/wide"),
		want: []string{
			"replaces|the one in GH_TOKEN — kept, takes this approval",
			"|  1 route unchanged",
			"|- " + r("/narrow"),
			"|+ " + r("/wide"),
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			useAs := bindingplan.Replace
			if tc.delegate {
				useAs = ""
			}
			fields := ReplacedFields(tc.standing, tc.delegate, "GH_TOKEN", useAs, tc.asked, tc.secret)
			if got, want := replacedRows(fields), strings.Join(tc.want, "\n"); got != want {
				t.Errorf("fields:\n%s\nwant:\n%s", got, want)
			}
			for _, f := range fields {
				want := CardAlert
				switch {
				case strings.HasPrefix(f.Value, "+ "):
					want = CardGood
				case strings.HasPrefix(f.Value, "  "):
					want = CardQuiet
				}
				if f.Tone != want {
					t.Errorf("%q has tone %v, want %v", f.Value, f.Tone, want)
				}
			}
		})
	}
}

// What a request row says it is about. The proxy's reactive path names
// nothing — all it saw was a sentinel it could not resolve — and a person's
// ask is not that.
func TestARequestRowSaysWhatItIsAbout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		req  CredentialRequest
		want string
	}{
		{"an agent names the credential", CredentialRequest{Name: "github", EnvVar: "GH_TOKEN", SandboxID: "sbx_1"}, "github"},
		{"an ask to delegate says so", CredentialRequest{Name: "github", SandboxID: "sbx_1", Delegate: &RequestPart{}}, "github · to delegate"},
		{"an ask for both says so", CredentialRequest{Name: "github", EnvVar: "GH_TOKEN", SandboxID: "sbx_1", Use: &RequestPart{}, Delegate: &RequestPart{}}, "github · to use and to delegate"},
		{"failing that, the variable", CredentialRequest{EnvVar: "GH_TOKEN", SandboxID: "sbx_1"}, "GH_TOKEN"},
		{"a discobox asked for a credential it did not name", CredentialRequest{SandboxID: "sbx_1"}, "a credential"},
	} {
		if got := RequestSubject(tc.req); got != tc.want {
			t.Errorf("%s: subject = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := Requester(CredentialRequest{}); got != "control plane" {
		t.Errorf("requester = %q, want the control plane", got)
	}
	if got := Requester(CredentialRequest{SandboxID: "sbx_abcdefghij"}); got != "agent · abcdefgh" {
		t.Errorf("requester = %q, want the agent and a short id", got)
	}
}

// The refused login's remedy does not send the person back into the box it
// showed in, which may be what provoked it, and says what a save shares.
func TestTheSignInAgainDialogNamesAFreshBoxAndWhatASaveShares(t *testing.T) {
	t.Parallel()
	_, body := SignInAgain(SecretRejection{HarnessConfigID: "hc_claude", HarnessConfigName: "claude", SecretID: "sec_claude"})
	for _, want := range []string{"new discobox", "rather than the box this refusal showed in", "can run commands"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dialog body %q does not say %q", body, want)
		}
	}
	if strings.Contains(body, "in this discobox") {
		t.Fatalf("dialog body %q sends the person back into this box", body)
	}
}
