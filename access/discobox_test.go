package access

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/agentcreds"
	"github.com/discobox-ai/discobox/policy"
)

// workerFacts is a discobox a lead created from discobox-ai/discobox at main,
// with a second source that has no network remote.
func workerFacts() agentcreds.FactsResponse {
	facts := policy.Facts{
		Discobox: policy.Discobox{ID: "sbx_worker", CreatedBy: "sbx_lead", Project: "default"},
		Sources: []policy.Source{
			{
				Slug: "primary", Primary: true,
				Remote: &policy.Remote{Host: "github.com", Repo: "discobox-ai/discobox", URL: "https://github.com/discobox-ai/discobox"},
				Ref:    &policy.Ref{Type: "branch", Name: "main"}, Commit: "0123456789abcdef0123456789abcdef01234567",
			},
			{Slug: "notes"},
		},
	}
	return agentcreds.FactsResponse{Facts: facts, Variables: facts.Variables()}
}

// facts shows the record and every variable, the unresolved ones saying so,
// and says how a policy names the primary source.
func TestFactsShowTheRecordAndEveryVariable(t *testing.T) {
	serve(t, &fakeService{facts: workerFacts()})
	stdout, stderr, code := capture(t, "", func() int { return Run([]string{"facts"}) })
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	for _, want := range []string{
		"This discobox is sbx_worker, in project default, created by discobox sbx_lead.",
		"This discobox's primary source is github.com/discobox-ai/discobox at branch main (commit 0123456789ab); write $primary.repo for discobox-ai/discobox.",
		"  notes: no network remote\n",
		"$primary.repo",
		"discobox-ai/discobox\n",
		"$primary.upstream",
		"(resolves to nothing, so matches nothing)",
		"$sources.notes.repo",
		"never\nfrom its git config",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("facts output lacks %q:\n%s", want, stdout)
		}
	}
}

// facts --json is the protocol's answer as is, every variable present even
// when it resolves to nothing.
func TestFactsJSONIsTheServicesAnswer(t *testing.T) {
	want := workerFacts()
	serve(t, &fakeService{facts: want})
	stdout, _, code := capture(t, "", func() int { return Run([]string{"facts", "--json"}) })
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	var got agentcreds.FactsResponse
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON (%v): %s", err, stdout)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("facts =\n%+v\nwant\n%+v", got, want)
	}
	if !strings.Contains(stdout, `{"name":"$primary.upstream","resolved":false}`) {
		t.Fatalf("an unresolved variable is not listed as one: %s", stdout)
	}
}

// list leads with what the discobox works on, in text and in JSON.
func TestListLeadsWithThePrimarySource(t *testing.T) {
	serve(t, &fakeService{facts: workerFacts()})
	stdout, _, code := capture(t, "", func() int { return Run([]string{"list"}) })
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.HasPrefix(stdout, "This discobox's primary source is github.com/discobox-ai/discobox at branch main") || !strings.Contains(stdout, Name+" facts") {
		t.Fatalf("list does not lead with the primary source:\n%s", stdout)
	}
	stdout, _, _ = capture(t, "", func() int { return Run([]string{"list", "--json"}) })
	var decoded listOutput
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not JSON (%v): %s", err, stdout)
	}
	if decoded.Discobox == nil || !reflect.DeepEqual(*decoded.Discobox, workerFacts()) {
		t.Fatalf("list --json discobox = %+v, want the facts", decoded.Discobox)
	}
}

// A service from before facts still lists its credentials, and facts says
// the verb is missing rather than that there is nothing to show.
func TestAServiceWithoutFactsStillLists(t *testing.T) {
	serve(t, &fakeService{predatesFacts: true, credentials: []agentcreds.Credential{{
		Name: "github", EnvVar: "GH_TOKEN", Hosts: []string{"github.com"},
		Uses: []agentcreds.Use{{UseID: "use_1", Description: "Open a PR"}},
	}}})
	stdout, stderr, code := capture(t, "", func() int { return Run([]string{"list", "--json"}) })
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if strings.Contains(stdout, `"discobox"`) || !strings.Contains(stdout, "use_1") {
		t.Fatalf("list = %s, want the credentials and no facts", stdout)
	}
	_, stderr, code = capture(t, "", func() int { return Run([]string{"facts", "--json"}) })
	if code != exitError || !strings.Contains(stderr, `"code":"not_found"`) || !strings.Contains(stderr, "404 page not found") || !strings.Contains(stderr, "predates") {
		t.Fatalf("facts exit = %d, stderr %s; want not_found saying the service predates it", code, stderr)
	}
}

// A discobox with no primary source, or one with no network remote, is told
// $primary.repo resolves to nothing rather than shown a repository.
func TestAPrimaryWithNoRepositorySaysSo(t *testing.T) {
	for name, tc := range map[string]struct {
		facts policy.Facts
		want  string
	}{
		"no primary":   {policy.Facts{Discobox: policy.Discobox{ID: "sbx_1"}}, "This discobox has no primary source: the $primary variables resolve to nothing"},
		"local remote": {policy.Facts{Discobox: policy.Discobox{ID: "sbx_1"}, Sources: []policy.Source{{Slug: "primary", Primary: true}}}, "has no network remote: $primary.repo and $primary.host resolve to nothing"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := primarySentence(tc.facts); !strings.Contains(got, tc.want) {
				t.Fatalf("primarySentence = %q, want %q", got, tc.want)
			}
		})
	}
}

// A facts read that fails for any reason leaves list listing: the facts are
// its header, and the credentials its answer.
func TestListListsWhenFactsFail(t *testing.T) {
	serve(t, &failingFacts{fakeService{credentials: []agentcreds.Credential{{
		Name: "github", EnvVar: "GH_TOKEN", Hosts: []string{"github.com"},
		Uses: []agentcreds.Use{{UseID: "use_1", Description: "Open a PR"}},
	}}}})
	stdout, stderr, code := capture(t, "", func() int { return Run([]string{"list"}) })
	if code != exitOK || !strings.Contains(stdout, "use_1") {
		t.Fatalf("list exit = %d, stdout %s; want the credentials", code, stdout)
	}
	if !strings.Contains(stderr, "could not read this discobox's facts") {
		t.Fatalf("stderr = %q, want why the header is missing", stderr)
	}
	stdout, _, code = capture(t, "", func() int { return Run([]string{"list", "--json"}) })
	if code != exitOK || !strings.Contains(stdout, "use_1") || strings.Contains(stdout, `"discobox"`) {
		t.Fatalf("list --json exit = %d, stdout %s; want the credentials and no facts", code, stdout)
	}
}

// failingFacts is a service whose facts read fails as an unreachable control
// plane does.
type failingFacts struct{ fakeService }

func (f *failingFacts) Facts(context.Context) (agentcreds.FactsResponse, error) {
	return agentcreds.FactsResponse{}, errors.New("control plane returned 503: unavailable")
}
