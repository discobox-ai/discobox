package console

import (
	"reflect"
	"testing"

	"github.com/discobox-ai/discobox/pkg/sandboxservices"
)

// The header is read off the box: the work it is tagged with, git, the diff,
// what it serves and the rest of its tags, each said once.
func TestHeaderSaysEachThingOnce(t *testing.T) {
	box := Sandbox{
		ID: "sbx_1", Repository: "https://github.com/acme/foo", Tags: []string{"pr=12", "ready", "wip", "issue=0"},
		Commit: "000", Git: GitState{Known: true, Branch: "fix", Commit: "abc"},
		Diff: DiffStat{Known: true, Added: 2, Deleted: 1, Files: 1},
	}
	h := NewHeader(box, nil)
	if h.ID != "sbx_1" || len(h.Work) != 1 || h.Work[0] != (Link{Label: "PR #12", URL: "https://github.com/acme/foo/pull/12", Issue: 12, Pull: true}) {
		t.Fatalf("work %+v", h.Work)
	}
	// A status tag is said by the git word; issue=0 numbers nothing, so it
	// is a tag like any other.
	if !reflect.DeepEqual(h.Tags, []string{"#wip", "#issue=0"}) {
		t.Fatalf("tags %q", h.Tags)
	}
	if h.Diff == nil || h.Diff.Added != 2 || h.Blocked {
		t.Fatalf("header %+v", h)
	}
	// Committed work beyond the spawn commit, under a ready pull request.
	if h.Base != "fix@abc⇡" || h.Changes != "ready" || h.Tone != ToneAhead || !h.Ready {
		t.Fatalf("git %q %q tone %v ready %v", h.Base, h.Changes, h.Tone, h.Ready)
	}
	box.Tags = append(box.Tags, "blocked")
	if h := NewHeader(box, nil); !h.Blocked || h.Changes != "" || h.Ready {
		t.Fatalf("a blocked box's git word: %+v", h)
	}
}

// Only a forwarded web port links, to its local end; the desktop is its own
// link once forwarded, and never a number among the ports.
func TestHeaderPorts(t *testing.T) {
	box := Sandbox{Ports: []Port{
		{Number: 6900, Protocol: "http", ServiceID: sandboxservices.DesktopID, ServiceName: "Browser desktop"},
		{Number: 53, UDP: true, Protocol: "udp"},
		{Number: 8443, Protocol: "https"},
		{Number: 3000, Protocol: "http"},
		{Number: 9000, Protocol: "unknown"},
	}}
	h := NewHeader(box, nil)
	if h.Desktop != nil {
		t.Fatalf("an unforwarded desktop is offered: %+v", h.Desktop)
	}
	forwarded := Forwarded([]Binding{{Port: 6900, Local: 6901}, {Port: 3000, Local: 3000}, {Port: 53, UDP: true, Local: 5353}})
	h = NewHeader(box, forwarded)
	if h.Desktop == nil || *h.Desktop != (Link{Label: "Browser desktop", URL: "http://localhost:6901"}) {
		t.Fatalf("desktop %+v", h.Desktop)
	}
	want := []PortGroup{
		{Protocol: "http", Ports: []Link{{Label: "3000", URL: "http://localhost:3000"}}},
		{Protocol: "https", Ports: []Link{{Label: "8443"}}},
		{Protocol: "udp", Ports: []Link{{Label: "5353->53"}}},
		{Protocol: "?", Ports: []Link{{Label: "9000"}}},
	}
	if !reflect.DeepEqual(h.Ports, want) {
		t.Fatalf("ports %+v, want %+v", h.Ports, want)
	}
}
