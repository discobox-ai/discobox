package sandboxcreate

import (
	"strings"
	"testing"
)

// --grant names a secret for the new discobox and the variable it receives
// it in; anything that is not SECRET[@HOST]:ENV_VAR is refused with the shape
// it should have had.
func TestGrantFlagsBecomeTheNewDiscoboxsGrants(t *testing.T) {
	grants, err := ParseGrants([]string{"github@github.com:GH_TOKEN", "npm:NPM_TOKEN"})
	if err != nil {
		t.Fatalf("ParseGrants: %v", err)
	}
	if len(grants) != 2 {
		t.Fatalf("grants = %+v, want two", grants)
	}
	if gh := grants[0]; gh.SecretId.Or("") != "github" || gh.EnvVar.Or("") != "GH_TOKEN" || strings.Join(gh.Hosts, ",") != "github.com" {
		t.Fatalf("github grant = %+v", gh)
	}
	if npm := grants[1]; npm.SecretId.Or("") != "npm" || npm.Hosts != nil {
		t.Fatalf("npm grant = %+v, want the secret's own host", npm)
	}

	for _, bad := range []string{"github", "com.github.api", ":GH_TOKEN", "github:", "@github.com:GH_TOKEN", "github:GH_TOKEN=push a branch"} {
		if _, err := ParseGrants([]string{bad}); err == nil || !strings.Contains(err.Error(), "SECRET[@HOST[,HOST...]]:ENV_VAR") {
			t.Fatalf("--grant %q: err = %v, want the shape it should have had", bad, err)
		}
	}
}

// A credential sent to more than one site names each, joined by commas
// (ADR 26-10-02-393), trimmed, lowercased and without repeats.
func TestAGrantMayNameSeveralHosts(t *testing.T) {
	grants, err := ParseGrants([]string{"github@ api.github.com, GitHubCopilot.com,api.github.com:COPILOT_GITHUB_TOKEN"})
	if err != nil {
		t.Fatalf("ParseGrants: %v", err)
	}
	if got := strings.Join(grants[0].Hosts, ","); got != "api.github.com,githubcopilot.com" {
		t.Fatalf("grant hosts = %q; want api.github.com, then githubcopilot.com", got)
	}
}

// One variable holds one credential.
func TestAVariableIsGivenOnce(t *testing.T) {
	if _, err := ParseGrants([]string{"github:GH_TOKEN", "other@github.com:GH_TOKEN"}); err == nil || !strings.Contains(err.Error(), "GH_TOKEN is already given") {
		t.Fatalf("err = %v, want the repeated variable refused", err)
	}
}
