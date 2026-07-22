package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	apimodel "github.com/obot-platform/discobox/api/model"
)

func TestPickOneWithoutChoices(t *testing.T) {
	cmd := &cobra.Command{}
	_, err := pickOne(cmd, "Select a sandbox", nil, pickerLabels{empty: "nothing here", ambiguous: "too many"})
	if err == nil || err.Error() != "nothing here" {
		t.Fatalf("pickOne err = %v, want the empty label", err)
	}
}

func TestPickOneWithSingleChoiceSkipsPrompt(t *testing.T) {
	cmd := &cobra.Command{}
	items := []pickerItem{{id: "sbx_1", title: "only"}}
	id, err := pickOne(cmd, "Select a sandbox", items, pickerLabels{empty: "nothing here", ambiguous: "too many"})
	if err != nil {
		t.Fatalf("pickOne: %v", err)
	}
	if id != "sbx_1" {
		t.Fatalf("pickOne id = %q, want sbx_1", id)
	}
}

// Without a terminal there is nobody to ask, so an ambiguous pick must fail
// with the caller's guidance rather than silently choosing.
func TestPickOneWithoutTerminalIsAmbiguous(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(""))
	cmd.SetErr(&bytes.Buffer{})
	items := []pickerItem{{id: "sbx_1"}, {id: "sbx_2"}}
	_, err := pickOne(cmd, "Select a sandbox", items, pickerLabels{empty: "nothing here", ambiguous: "too many"})
	if err == nil || err.Error() != "too many" {
		t.Fatalf("pickOne err = %v, want the ambiguous label", err)
	}
}

func TestSandboxPickerItemsAreOldestFirst(t *testing.T) {
	older := apimodel.Sandbox{ID: "sbx_old", CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}
	older.Config.Name = "older"
	newer := apimodel.Sandbox{ID: "sbx_new", CreatedAt: time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)}
	newer.Config.Name = "newer"

	items := sandboxPickerItems([]apimodel.Sandbox{newer, older})
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].id != "sbx_old" || items[1].id != "sbx_new" {
		t.Fatalf("items order = %q, %q, want oldest first", items[0].id, items[1].id)
	}
	if items[0].title != "older" {
		t.Fatalf("item title = %q, want older", items[0].title)
	}
}
