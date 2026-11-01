package cli

import (
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

func selectSourcesTestSandbox(skillsOnly bool) *apimodel.Sandbox {
	sandbox := &apimodel.Sandbox{}
	if !skillsOnly {
		primary := apiclientgen.GitSource{Kind: apiclientgen.GitSourceKindGit}
		primary.SetSlug(apiclientgen.NewOptString("primary"))
		sandbox.Config.SetSource(apiclientgen.NewOptGitSource(primary))
	}
	skills := apiclientgen.GitSource{Kind: apiclientgen.GitSourceKindGit}
	skills.SetSlug(apiclientgen.NewOptString("tools"))
	skills.SetPurpose(apiclientgen.NewOptGitSourcePurpose(apiclientgen.GitSourcePurposeSkills))
	sandbox.Config.SetSourceCodeReferences(apiclientgen.NewOptSandboxConfigSourceCodeReferences(
		apiclientgen.SandboxConfigSourceCodeReferences{"~/.discobox/skill-sources/tools": skills}))
	return sandbox
}

// Push acts on the sources being worked on: a skills source is left
// out unless it is named, and a sandbox with no other source says so.
func TestSelectSourcesLeavesSkillsSourcesOutUnlessNamed(t *testing.T) {
	sources, err := selectSources(selectSourcesTestSandbox(false), "")
	if err != nil || len(sources) != 1 || sources[0].slug != "primary" {
		t.Fatalf("select sources = %+v, %v; want the primary alone", sources, err)
	}
	sources, err = selectSources(selectSourcesTestSandbox(false), "tools")
	if err != nil || len(sources) != 1 || sources[0].slug != "tools" {
		t.Fatalf("select sources tools = %+v, %v; want the named skills source", sources, err)
	}
	if _, err := selectSources(selectSourcesTestSandbox(true), ""); err == nil || !strings.Contains(err.Error(), "--source") {
		t.Fatalf("select sources of a skills-only sandbox = %v, want a refusal naming --source", err)
	}
}
