package cli

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"
)

// A command that could not reach the server names the command that says why.
// The diagnosis is not at the top level (ADR 0112), so the failure is where
// somebody finds out it exists — and the failure is the moment they need it.
func TestUnreachableServerErrorNamesTheDiagnosis(t *testing.T) {
	root, _ := newRootCommand()
	list, _, err := root.Find([]string{"ls"})
	if err != nil {
		t.Fatalf("Find(ls) error = %v", err)
	}
	// What the stack actually produces: the client's mark around the dial that
	// failed, wrapped by http.Client in the URL it was for.
	dial := &url.Error{
		Op:  "Get",
		URL: "http://discobox.local/api/projects",
		Err: serverUnreachable{err: &net.OpError{Op: "dial", Net: "unix", Err: syscall.ECONNREFUSED}},
	}

	hinted := withUnreachableServerHint(list, dial)
	if !strings.Contains(hinted.Error(), "admin server status") {
		t.Fatalf("error = %q, want it to name the diagnosis", hinted)
	}
	// The transport's own words are still the error, and still reachable: the
	// hint is added to the failure, not swapped for it.
	if !errors.Is(hinted, dial) {
		t.Fatalf("error = %q, want the original failure wrapped", hinted)
	}

	// A server that answered has already said what is wrong with the request,
	// and pointing at a connection diagnosis would send the reader the wrong
	// way entirely.
	answered := errors.New("request failed: 403 Forbidden")
	if got := withUnreachableServerHint(list, answered); got.Error() != answered.Error() {
		t.Fatalf("error = %q, want the server's own answer unchanged", got)
	}

	// Another request that could not connect is not this one. `admin server
	// stage` downloads a release asset, and it is run by somebody who has no
	// server yet — pointing them at a control-plane diagnosis would send them
	// after a server that may be answering perfectly, or that they have not
	// installed at all.
	stage, _, err := root.Find([]string{"admin", "server", "stage"})
	if err != nil {
		t.Fatalf("Find(admin server stage) error = %v", err)
	}
	download := &url.Error{
		Op:  "Get",
		URL: "https://assets.discobox.ai/v1.2.3/discobox-server-linux-amd64",
		Err: syscall.ECONNREFUSED,
	}
	if got := withUnreachableServerHint(stage, download); got.Error() != download.Error() {
		t.Fatalf("error = %q, want an unrelated download failure left alone", got)
	}
	// An interrupt is not an unreachable server either, even though it reaches
	// the same transport.
	interrupted := &url.Error{
		Op:  "Get",
		URL: "http://discobox.local/api/projects",
		Err: serverUnreachable{err: context.Canceled},
	}
	if got := withUnreachableServerHint(list, interrupted); got.Error() != interrupted.Error() {
		t.Fatalf("error = %q, want no hint for an interrupted command", got)
	}

	// And the diagnosis does not tell anybody to run the diagnosis.
	status, _, err := root.Find([]string{"admin", "server", "status"})
	if err != nil {
		t.Fatalf("Find(admin server status) error = %v", err)
	}
	if got := withUnreachableServerHint(status, dial); got.Error() != dial.Error() {
		t.Fatalf("status error = %q, want no hint naming itself", got)
	}
}
