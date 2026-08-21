package cli

import (
	"strings"
	"testing"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
)

func TestParseCopyItems(t *testing.T) {
	items, err := parseCopyItems(copyableResources)
	if err != nil {
		t.Fatalf("parseCopyItems: %v", err)
	}
	if len(items) != 3 || items[0] != apiclientgen.CreateProjectBodyCopyItemProviders {
		t.Fatalf("items = %#v, want all three copyable resources", items)
	}

	// "none" is how the flag's non-empty default is turned off, and it must
	// produce an empty selection rather than an error or a nil-ish default.
	items, err = parseCopyItems([]string{"none"})
	if err != nil {
		t.Fatalf("parseCopyItems(none): %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %#v, want none", items)
	}

	if _, err := parseCopyItems([]string{"sandboxes"}); err == nil {
		t.Fatal("parseCopyItems(sandboxes) error = nil, want unknown value")
	}
}

// --copy without --from names a source that does not exist, so it is a
// mistake worth reporting rather than a silently ignored flag.
func TestProjectCreateRejectsCopyWithoutFrom(t *testing.T) {
	cmd := NewRootCommand()
	cmd.SetArgs([]string{"admin", "project", "create", "Thing", "--copy", "pools"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--copy needs --from") {
		t.Fatalf("execute error = %v, want --copy needs --from", err)
	}
}
