package console

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A URL printed in a box is pointed where it reaches from here: the box's
// port, answered with the local one the forward bound, on the name rather
// than the address.
func TestForwardedURLPointsAtTheLocalEnd(t *testing.T) {
	t.Parallel()
	forwarded := func() map[PortKey]int {
		return Forwarded([]Binding{{Port: 8080, Local: 8081}, {Port: 443, Local: 8443}, {Port: 80, Local: 8000}, {Port: 9000, Local: 9001, UDP: true}})
	}
	for _, tc := range []struct{ raw, want string }{
		// The port moved, so the URL does.
		{"http://localhost:8080/", "http://localhost:8081/"},
		// The bind address a server prints is not one to open; the port is
		// still the sandbox's, and the answer is the same either way.
		{"http://0.0.0.0:8080/health?x=1", "http://localhost:8081/health?x=1"},
		{"http://127.0.0.1:8080", "http://localhost:8081"},
		{"http://[::1]:8080/", "http://localhost:8081/"},
		// A port left out is the scheme's, and forwardable like any other.
		{"https://localhost/admin", "https://localhost:8443/admin"},
		{"http://localhost/", "http://localhost:8000/"},
		// Everything else is already right, and saying so is what leaves it as
		// text for the terminal's own linking.
		{"http://localhost:9999/", "http://localhost:9999/"},
		{"http://localhost:9000/", "http://localhost:9000/"},
		{"https://github.com/discobox-ai/discobox", "https://github.com/discobox-ai/discobox"},
		{"postgres://localhost:8080/db", "postgres://localhost:8080/db"},
		{"http://example.com:8080/", "http://example.com:8080/"},
		{"discobox:///sbx_two", "discobox:///sbx_two"},
	} {
		if got := ForwardedURL(tc.raw, forwarded); got != tc.want {
			t.Errorf("ForwardedURL(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// The forward's bindings are asked for only once a URL could be the box's:
// most URLs on a screen are somewhere else, and nothing forwarded moves none.
func TestForwardedURLAsksTheForwardOnlyForTheBoxsOwn(t *testing.T) {
	t.Parallel()
	asked := 0
	forwarded := func() map[PortKey]int { asked++; return nil }
	if got := ForwardedURL("https://example.com/", forwarded); got != "https://example.com/" || asked != 0 {
		t.Fatalf("ForwardedURL = %q after %d asks, want it unmoved and the forward unasked", got, asked)
	}
	if got := ForwardedURL("http://localhost:8080/", forwarded); got != "http://localhost:8080/" || asked != 1 {
		t.Fatalf("ForwardedURL = %q after %d asks, want it unmoved with nothing forwarded", got, asked)
	}
}

// A box link is a discobox's address, or says why it is not one, quoting
// what the program wrote.
func TestParseBoxLink(t *testing.T) {
	t.Parallel()
	address, err := ParseBoxLink("discobox:///sbx_two")
	if err != nil || !address.Local() || address.Sandbox != "sbx_two" {
		t.Fatalf("ParseBoxLink = %+v, %v; want sbx_two on the reader's server", address, err)
	}
	if _, err := ParseBoxLink("https://example.com/"); err == nil || !strings.Contains(err.Error(), `"https://example.com/" is not a discobox's address`) {
		t.Fatalf("ParseBoxLink of a web URL = %v", err)
	}
	if _, err := ParseBoxLink("discobox:///sbx_two/extra\x1b"); err == nil || strings.Contains(err.Error(), "\x1b") {
		t.Fatalf("ParseBoxLink = %v, want a refusal with the escape quoted", err)
	}
}

// The reader's server is the pane's box's, and the primary for a pane that is
// no box's.
func TestLinkReaderServer(t *testing.T) {
	t.Parallel()
	if got := LinkReaderServer("beta", true, "alpha"); got != "beta" {
		t.Fatalf("a box's pane reads on %q, want beta", got)
	}
	if got := LinkReaderServer("", true, "alpha"); got != "" {
		t.Fatalf("a pane of the one server's box reads on %q, want it", got)
	}
	if got := LinkReaderServer("", false, "alpha"); got != "alpha" {
		t.Fatalf("a pane that is no box's reads on %q, want alpha", got)
	}
}

type addressedSource struct {
	DataSource
	name   string
	known  bool
	unsure error
}

func (s addressedSource) AddressedServer(context.Context, string) (string, bool, error) {
	return s.name, s.known, s.unsure
}

// A link naming a server is on the listed server that says it is that one;
// one the window does not list is refused with how to register it, and one a
// listed server may be says that instead. A link naming none is the reader's.
func TestBoxLinkServer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	full, _ := ParseBoxLink("discobox://elsewhere.example.com/sbx_two")
	local, _ := ParseBoxLink("discobox:///sbx_two")

	if got, err := BoxLinkServer(ctx, addressedSource{}, local, "alpha", "u"); got != "alpha" || err != nil {
		t.Fatalf("a local link is on %q, %v; want the reader's", got, err)
	}
	if got, err := BoxLinkServer(ctx, addressedSource{name: "beta", known: true}, full, "alpha", "u"); got != "beta" || err != nil {
		t.Fatalf("a listed server's link is on %q, %v; want beta", got, err)
	}
	if _, err := BoxLinkServer(ctx, addressedSource{}, full, "alpha", "u"); err == nil || !strings.Contains(err.Error(), "`discobox attach \"u\"` registers it") {
		t.Fatalf("an unknown server's link = %v", err)
	}
	_, err := BoxLinkServer(ctx, addressedSource{unsure: errors.New("gamma did not say")}, full, "alpha", "u")
	if err == nil || !strings.Contains(err.Error(), "gamma did not say") || strings.Contains(err.Error(), "doesn't know") {
		t.Fatalf("a doubtful server's link = %v", err)
	}
}

// A link's box is the one on its server with that ID, or the one whose ID it
// is the only prefix of; otherwise it says why there is none.
func TestAddressedBox(t *testing.T) {
	t.Parallel()
	boxes := []Sandbox{{ID: "sbx_one", Server: "alpha"}, {ID: "sbx_onex", Server: "alpha"}, {ID: "sbx_two", Server: "alpha"}, {ID: "sbx_two", Server: "beta"}}
	for _, tc := range []struct {
		server, ref, want, why string
		unreachable            []string
	}{
		{server: "beta", ref: "sbx_two", want: "beta/sbx_two"},
		{server: "alpha", ref: "sbx_one", want: "alpha/sbx_one"},
		{server: "alpha", ref: "sbx_t", want: "alpha/sbx_two"},
		{server: "alpha", ref: "sbx_o", why: `"sbx_o" is more than one discobox on alpha`},
		{server: "alpha", ref: "sbx_nope", why: `no discobox "sbx_nope" on alpha`},
		{server: "", ref: "sbx_nope", why: `no discobox "sbx_nope" on this server`},
		{server: "gamma", ref: "sbx_two", unreachable: []string{"gamma"}, why: "gamma isn't answering"},
	} {
		got, err := AddressedBox(boxes, tc.unreachable, tc.server, tc.ref, "u")
		switch {
		case tc.why != "":
			if err == nil || !strings.Contains(err.Error(), tc.why) {
				t.Errorf("AddressedBox(%s, %s) = %v, want %q", tc.server, tc.ref, err, tc.why)
			}
		case err != nil || got.Server+"/"+got.ID != tc.want:
			t.Errorf("AddressedBox(%s, %s) = %s/%s, %v; want %s", tc.server, tc.ref, got.Server, got.ID, err, tc.want)
		}
	}
}
