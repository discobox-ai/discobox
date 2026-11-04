package tui

import (
	"strings"
	"testing"
)

func TestAdoptionCardNamesAuthorityAndWiderEgress(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	req := CredentialRequest{ID: "adopt_1", Justification: "lead the feature", Adoption: &AdoptionAsk{ChildID: "child", ChildName: "feature", RequesterID: "lead", CurrentParentID: "old-lead", ReadOnly: true}}
	m.openCredentialRequest(req)
	text := dialogText(m)
	for _, want := range []string{"child", "lead", "old-lead", "lead the feature", "terminals", "deliver its source", "wider egress", "only read", "credentials stay its own"} {
		if !strings.Contains(text, want) {
			t.Fatalf("card missing %q: %s", want, text)
		}
	}
}
