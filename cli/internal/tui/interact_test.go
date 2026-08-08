package tui

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// A command-shaped action still runs with the real terminal's streams while the
// window is suspended: the runtime drops out of the alternate screen around it,
// so a pager has the screen the window was started from to page onto.
func TestInteractRunsWithTheRealStreams(t *testing.T) {
	ds := newFakeSource()
	var out bytes.Buffer
	c := &interactExec{ctx: context.Background(), ds: ds, action: InteractDiff, ids: []string{"sbx_one"}}
	c.SetStdin(strings.NewReader(""))
	c.SetStdout(&out)
	c.SetStderr(io.Discard)

	if err := c.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(ds.interacts) != 1 || ds.interacts[0] != "diff sbx_one" {
		t.Fatalf("interacts = %v", ds.interacts)
	}
	// Nothing of its own is written: the runtime has already put the screen
	// back, so there are no rows to reclaim.
	if out.Len() != 0 {
		t.Fatalf("wrote %q, want nothing", out.String())
	}
}
