package bridge

import (
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// follow sends a link pressed in a pane of box (empty for no box's) and
// returns where the core says it goes.
func (a *app) follow(id uint64, uri, box string) Result {
	a.t.Helper()
	a.send(Message{Type: TypeFollow, Follow: &Follow{ID: id, URI: uri, Box: box}})
	return a.result(id)
}

// A URL printed in a box goes to the port forwarded here for its header, for
// as long as the header holds the forward; any other web address is opened as written.
func TestAFollowedURLGoesThroughTheHeadersForward(t *testing.T) {
	core := newFakeCore("home", running("b1"), running("b2"))
	core.forward = newFakeForward(console.Binding{Port: 8080, Local: 8081})
	a := dial(t, core)
	a.hello()

	if r := a.follow(100, "http://localhost:8080/x", "b1"); r.Status != StatusDone || r.URL != "http://localhost:8080/x" {
		t.Fatalf("with no header followed: %+v", r)
	}
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicHeader, Box: "b1"}})
	a.expect(TypeSnapshot)
	for _, tc := range []struct{ uri, box, want string }{
		{"http://localhost:8080/x?y=1", "b1", "http://localhost:8081/x?y=1"},
		{"http://0.0.0.0:8080", "b1", "http://localhost:8081"},
		// Another box's port is its own, and a pane that is no box's names
		// none.
		{"http://localhost:8080/x", "b2", "http://localhost:8080/x"},
		{"http://localhost:8080/x", "", "http://localhost:8080/x"},
		{"https://example.com/a", "b1", "https://example.com/a"},
		{"HTTP://example.com/a", "b1", "HTTP://example.com/a"},
	} {
		if r := a.follow(101, tc.uri, tc.box); r.Status != StatusDone || r.URL != tc.want {
			t.Errorf("follow %q in %q: %+v, want %q", tc.uri, tc.box, r, tc.want)
		}
	}
	a.send(Message{Type: TypeUnsubscribe, Unsubscribe: &Unsubscribe{Sub: 1}})
	eventually(t, "the forward was let go", func() bool {
		return a.follow(102, "http://localhost:8080/x", "b1").URL == "http://localhost:8080/x"
	})
}

// A discobox link enters the box it names: one naming no server on the
// server of the box whose pane it was in, one naming a server on the listed
// server that says it is that one; anything else fails, saying why.
func TestAFollowedBoxLinkEntersItsBox(t *testing.T) {
	home := newFakeCore("home")
	work := newFakeCore("work")
	home.servers["work"] = work
	home.addressed = map[string]string{"discobox://work.example.com": "work"}
	home.setListing(running("same"), running("sbx_alone"),
		console.Sandbox{ID: "same", Name: "same", Server: "work", State: console.StateRunning, HasRuntime: true})
	a := dial(t, home)
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicListing}})
	a.expect(TypeSnapshot)

	for _, tc := range []struct{ uri, box, want string }{
		{"discobox:///same", "work/same", "work/same"},
		{"discobox:///same", "same", "same"},
		{"discobox:///same", "", "same"},
		{"DISCOBOX:///sbx_al", "", "sbx_alone"},
		{"discobox://work.example.com/same", "same", "work/same"},
	} {
		if r := a.follow(200, tc.uri, tc.box); r.Status != StatusEnter || r.Box != tc.want {
			t.Errorf("follow %q in %q: %+v, want %q entered", tc.uri, tc.box, r, tc.want)
		}
	}
	// A box of a server the listing could not read is missing from it, and
	// its links are still on its server: never a box of the primary's.
	home.mu.Lock()
	home.listing = console.Listing{Sandboxes: []console.Sandbox{running("same"), running("sbx_alone")}, Unreachable: []string{"work"}}
	home.mu.Unlock()
	a.command(Command{Command: "refresh"})
	for {
		if l := a.expect(TypeSnapshot).Snapshot.Listing; l != nil && len(l.Unreachable) > 0 {
			break
		}
	}
	if r := a.follow(202, "discobox:///same", "work/same"); r.Status != StatusFailed || !strings.Contains(r.Why, "work isn't answering") {
		t.Errorf("follow in an unreachable server's pane: %+v", r)
	}
	for _, tc := range []struct{ uri, why string }{
		{"discobox:///nope", `no discobox "nope" on this server`},
		{"discobox://elsewhere.example.com/same", "`discobox attach"},
		{"discobox:///same/more", "discobox:///same/more"},
	} {
		if r := a.follow(201, tc.uri, ""); r.Status != StatusFailed || !strings.Contains(r.Why, tc.why) {
			t.Errorf("follow %q: %+v, want it refused with %q", tc.uri, r, tc.why)
		}
	}
}

// The desktop is the outer terminal: only web addresses may reach this
// machine's URL handler, even when an OSC 8 label looks like a web address.
func TestAFollowedNonWebLinkIsRefused(t *testing.T) {
	a := dial(t, newFakeCore("home"))
	a.hello()
	for _, uri := range []string{
		"file:///etc/passwd", "smb://host/share", "vscode://file/etc/passwd",
		"mailto:person@example.com", "javascript:alert(1)", "data:text/html,hello",
		"//example.com/path", "/tmp/file", "", "https:", "https:example.com",
		"https:///path", "http://%zz", "file:///tmp/\x1b[31m",
	} {
		if r := a.follow(300, uri, ""); r.Status != StatusFailed || r.URL != "" || !strings.Contains(r.Why, "is not a web address") {
			t.Errorf("follow %q: %+v, want refusal with no URL", uri, r)
		}
	}
}
