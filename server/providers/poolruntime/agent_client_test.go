package poolruntime

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

func TestPoolCreateRequestForwardsSourceDataKeys(t *testing.T) {
	primaryKey := strings.Repeat("a", 64)
	refKey := strings.Repeat("b", 64)
	primary := model.GitSource{Kind: "git"}
	request := poolCreateRequestFromOptions("sandbox-1", sandbox.CreateOptions{
		Source:                      &primary,
		SourceDataKey:               primaryKey,
		SourceCodeReferences:        model.SourceCodeReferences{"library": {Kind: "git"}},
		SourceCodeReferenceDataKeys: map[string]string{"library": refKey},
	})

	gotPrimary, ok := request.Config.Source.Get()
	if !ok || gotPrimary.DataKey.Or("") != primaryKey {
		t.Fatalf("primary data key = %q, want %q", gotPrimary.DataKey.Or(""), primaryKey)
	}
	refs, ok := request.Config.SourceCodeReferences.Get()
	if !ok || refs["library"].DataKey.Or("") != refKey {
		t.Fatalf("reference data key = %q, want %q", refs["library"].DataKey.Or(""), refKey)
	}
}
