package handlers

import (
	"slices"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/targetdb"
)

// The kinds the API offers are the kinds an entry may hold: an entry's
// documents and terms are the target package's types, stored whole, so no
// model tag ties them to server.yaml.
func TestTargetKindsAreTheAPIs(t *testing.T) {
	check := func(name string, api, own []string) {
		t.Helper()
		slices.Sort(api)
		slices.Sort(own)
		if !slices.Equal(api, own) {
			t.Errorf("%s: the API offers %v, the target package %v", name, api, own)
		}
	}
	var kinds, putKinds, ownKinds []string
	for _, k := range serverapi.TargetDocumentKind("").AllValues() {
		kinds = append(kinds, string(k))
	}
	for _, k := range serverapi.PutTargetDocumentKind("").AllValues() {
		putKinds = append(putKinds, string(k))
	}
	for _, k := range targetdb.Kinds {
		ownKinds = append(ownKinds, string(k))
	}
	check("TargetDocument.kind", kinds, ownKinds)
	check("PutTargetDocument.kind", putKinds, ownKinds)

	var termKinds, ownTermKinds []string
	for _, k := range serverapi.TargetTermKind("").AllValues() {
		termKinds = append(termKinds, string(k))
	}
	for _, k := range targetdb.TermKinds {
		ownTermKinds = append(ownTermKinds, string(k))
	}
	check("TargetTerm.kind", termKinds, ownTermKinds)
}
