package sandboxes

import (
	"reflect"
	"testing"

	"github.com/obot-platform/discobox/server/internal/model"
)

func satisfiedSet(names ...string) func(string) bool {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return func(name string) bool {
		_, ok := set[name]
		return ok
	}
}

func TestMissingRequiredSecrets(t *testing.T) {
	auth := []model.HarnessConfigSecret{
		{Name: "ANTHROPIC_API_KEY", Required: true, OneOfGroup: "auth"},
		{Name: "CLAUDE_CODE_OAUTH_TOKEN", Required: true, OneOfGroup: "auth"},
	}

	tests := []struct {
		name      string
		decls     []model.HarnessConfigSecret
		satisfied func(string) bool
		want      []string
	}{
		{
			name:      "one-of group satisfied by first member",
			decls:     auth,
			satisfied: satisfiedSet("ANTHROPIC_API_KEY"),
			want:      nil,
		},
		{
			name:      "one-of group satisfied by second member",
			decls:     auth,
			satisfied: satisfiedSet("CLAUDE_CODE_OAUTH_TOKEN"),
			want:      nil,
		},
		{
			name:      "one-of group unsatisfied reports alternatives",
			decls:     auth,
			satisfied: satisfiedSet(),
			want:      []string{"one of: ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN"},
		},
		{
			name: "ungrouped required still enforced independently",
			decls: []model.HarnessConfigSecret{
				{Name: "GITHUB_TOKEN", Required: true},
				{Name: "NPM_TOKEN", Required: true},
			},
			satisfied: satisfiedSet("GITHUB_TOKEN"),
			want:      []string{"NPM_TOKEN"},
		},
		{
			name: "optional secrets never reported",
			decls: []model.HarnessConfigSecret{
				{Name: "OPTIONAL_KEY", Required: false},
				{Name: "OPTIONAL_GROUPED", Required: false, OneOfGroup: "opt"},
			},
			satisfied: satisfiedSet(),
			want:      nil,
		},
		{
			name: "mixed grouped and ungrouped, both missing, deterministic order",
			decls: append([]model.HarnessConfigSecret{
				{Name: "GITHUB_TOKEN", Required: true},
			}, auth...),
			satisfied: satisfiedSet(),
			want:      []string{"GITHUB_TOKEN", "one of: ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := missingRequiredSecrets(tc.decls, tc.satisfied)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("missingRequiredSecrets = %#v, want %#v", got, tc.want)
			}
		})
	}
}
