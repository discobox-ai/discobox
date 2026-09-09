package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/sandboxservices"
)

// The protocol is the repetitive half of a port list, so it is said once per
// group rather than once per port: three dev servers should not spell "http"
// three times on a header row that is already short of space.
func TestPortsTextGroupsByProtocol(t *testing.T) {
	st := newStyles(false)
	for _, tc := range []struct {
		name  string
		ports []Port
		want  string
	}{
		{name: "nothing listening", ports: nil, want: ""},
		{
			name:  "one port",
			ports: []Port{{Number: 5173, Protocol: "http"}},
			want:  "http:5173",
		},
		{
			name: "a protocol is named once however many ports it has",
			ports: []Port{
				{Number: 3000, Protocol: "http"},
				{Number: 5173, Protocol: "http"},
				{Number: 8080, Protocol: "http"},
			},
			want: "http:3000,5173,8080",
		},
		{
			name: "groups run web first, whatever order they arrive in",
			ports: []Port{
				{Number: 22, Protocol: "tcp"},
				{Number: 3000, Protocol: "http"},
				{Number: 5432, Protocol: "tcp"},
				{Number: 8443, Protocol: "https"},
				{Number: 5173, Protocol: "http"},
				{Number: 6379, Protocol: "tcp"},
				{Number: 8080, Protocol: "http"},
			},
			want: "http:3000,5173,8080 · https:8443 · tcp:22,5432,6379",
		},
		{
			// The longest word for the least information, on the one port
			// where the number is all there is to say.
			name: "an unprobed port is a question mark",
			ports: []Port{
				{Number: 5173, Protocol: "http"},
				{Number: 9000, Protocol: "unknown"},
			},
			want: "http:5173 · ?:9000",
		},
		{
			// Version skew: a newer agent classifying something this CLI has
			// never heard of must not be renamed or dropped by it.
			name: "a protocol this CLI does not know keeps its name and follows",
			ports: []Port{
				{Number: 4000, Protocol: "grpc"},
				{Number: 5173, Protocol: "http"},
			},
			want: "http:5173 · grpc:4000",
		},
		{
			// The banner should not depend on the agent having sorted first.
			name: "numbers come out in order however they arrived",
			ports: []Port{
				{Number: 8080, Protocol: "http"},
				{Number: 3000, Protocol: "http"},
			},
			want: "http:3000,8080",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ansi.Strip(portsText(st, Sandbox{Ports: tc.ports}, nil)); got != tc.want {
				t.Fatalf("portsText = %q, want %q", got, tc.want)
			}
		})
	}
}

// A forwarded port that had to move shows both numbers, so the header says what
// to type here as well as what the sandbox is serving there. One that kept its
// number says it once: there is nothing to correct.
func TestPortsTextShowsTheLocalPortForForwardedPorts(t *testing.T) {
	st := newStyles(false)
	ports := []Port{
		{Number: 8080, Protocol: "http"},
		{Number: 3000, Protocol: "http"},
		{Number: 5432, Protocol: "tcp"},
	}
	// 8080 was taken locally and 3000 was not; 5432 is forwarded too, and says
	// so the same way even though nothing can link to it.
	forwarded := map[int]int{8080: 8082, 3000: 3000, 5432: 5433}
	want := "http:3000,8082->8080 · tcp:5433->5432"
	if got := ansi.Strip(portsText(st, Sandbox{Ports: ports}, forwarded)); got != want {
		t.Fatalf("portsText = %q, want %q", got, want)
	}
}

// A port the forward has not bound keeps its bare number: an arrow on it would
// promise a local port that is not listening.
func TestPortsTextLeavesUnforwardedPortsAlone(t *testing.T) {
	st := newStyles(false)
	ports := []Port{{Number: 8080, Protocol: "http"}, {Number: 9000, Protocol: "http"}}
	want := "http:8081->8080,9000"
	if got := ansi.Strip(portsText(st, Sandbox{Ports: ports}, map[int]int{8080: 8081})); got != want {
		t.Fatalf("portsText = %q, want %q", got, want)
	}
}

// The web ports carry an OSC 8 link to the local end of the forward, and
// nothing else does: a browser has nothing to do with a Postgres socket, and a
// port with no local end has nowhere to point.
func TestPortsTextLinksForwardedWebPorts(t *testing.T) {
	st := newStyles(false)
	ports := []Port{
		{Number: 8080, Protocol: "http"},
		{Number: 8443, Protocol: "https"},
		{Number: 5432, Protocol: "tcp"},
		{Number: 9000, Protocol: "http"},
	}
	rendered := portsText(st, Sandbox{Ports: ports}, map[int]int{8080: 8082, 8443: 8444, 5432: 5433})
	for _, want := range []string{
		hyperlink("http://localhost:8082", "8082->8080"),
		hyperlink("https://localhost:8444", "8444->8443"),
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("portsText = %q, want it to contain %q", rendered, want)
		}
	}
	for _, unwanted := range []string{"localhost:5433", "localhost:9000"} {
		if strings.Contains(rendered, unwanted) {
			t.Errorf("portsText = %q, want no link to %q", rendered, unwanted)
		}
	}
	// The escape sequences take no cells, so the row still measures as its text.
	if got, want := lipgloss.Width(rendered), lipgloss.Width(ansi.Strip(rendered)); got != want {
		t.Fatalf("width with links = %d, want %d — the sequences must not occupy cells", got, want)
	}
}

// A port the forward kept the number of is still forwarded, so it still links —
// what it drops is the arrow, not the local end.
func TestPortsTextLinksAForwardThatKeptItsNumber(t *testing.T) {
	st := newStyles(false)
	rendered := portsText(st, Sandbox{Ports: []Port{{Number: 5173, Protocol: "http"}}}, map[int]int{5173: 5173})
	if want := hyperlink("http://localhost:5173", "5173"); !strings.Contains(rendered, want) {
		t.Fatalf("portsText = %q, want it to contain %q", rendered, want)
	}
}

// The desktop is not a port somebody's program is serving, so it is kept out of
// the protocol groups: `http:6900` beside a dev server invites opening it as if
// it were one, and the number is not the useful thing about it.
func TestPortsTextLeavesTheDesktopOut(t *testing.T) {
	st := newStyles(false)
	ports := []Port{
		{Number: 8080, Protocol: "http"},
		{Number: 6900, Protocol: "http", ServiceID: sandboxservices.DesktopID, ServiceName: "Desktop"},
	}
	got := ansi.Strip(portsText(st, Sandbox{Ports: ports}, map[int]int{8080: 8080, 6900: 6900}))
	if want := "http:8080"; got != want {
		t.Fatalf("portsText = %q, want %q", got, want)
	}
}

// A sandbox serving only the desktop renders no port group at all, rather than
// an empty `http:` label.
func TestPortsTextIsEmptyWhenOnlyTheDesktopIsServed(t *testing.T) {
	st := newStyles(false)
	ports := []Port{{Number: 6900, Protocol: "http", ServiceID: sandboxservices.DesktopID}}
	if got := ansi.Strip(portsText(st, Sandbox{Ports: ports}, map[int]int{6900: 6900})); got != "" {
		t.Fatalf("portsText = %q, want empty", got)
	}
}

// The desktop gets its own field, labeled by the declaration rather than by a
// number, and linked to the local end of its forward.
func TestDesktopTextLinksTheForwardedDesktop(t *testing.T) {
	st := newStyles(false)
	ports := []Port{{Number: 6900, Protocol: "http", ServiceID: sandboxservices.DesktopID, ServiceName: "Desktop"}}

	rendered := desktopText(st, Sandbox{Ports: ports}, map[int]int{6900: 6901})
	if got := ansi.Strip(rendered); got != "Desktop" {
		t.Fatalf("desktopText = %q, want the declaration's name", got)
	}
	// The link points at the local end, which is the only end reachable here.
	if !strings.Contains(rendered, "http://localhost:6901") {
		t.Fatalf("desktopText did not link the forwarded port: %q", rendered)
	}
	if strings.Contains(ansi.Strip(rendered), "6900") {
		t.Fatalf("desktopText shows a port number: %q", rendered)
	}
}

// An offer to open a desktop that is not reachable is worse than no offer, so
// an unforwarded one draws nothing — the rule portEntry follows for links.
func TestDesktopTextIsEmptyWithoutAForward(t *testing.T) {
	st := newStyles(false)
	ports := []Port{{Number: 6900, Protocol: "http", ServiceID: sandboxservices.DesktopID, ServiceName: "Desktop"}}
	if got := desktopText(st, Sandbox{Ports: ports}, nil); got != "" {
		t.Fatalf("desktopText = %q, want empty with no forward", got)
	}
}

// Nothing here knows what 6900 is. A desktop declared on another port is still
// the desktop, and a plain port on 6900 is still a plain port.
func TestTheDesktopIsRecognizedByIDNotByPortNumber(t *testing.T) {
	st := newStyles(false)
	elsewhere := []Port{{Number: 7100, Protocol: "http", ServiceID: sandboxservices.DesktopID, ServiceName: "Desktop"}}
	if got := ansi.Strip(desktopText(st, Sandbox{Ports: elsewhere}, map[int]int{7100: 7100})); got != "Desktop" {
		t.Fatalf("a desktop on another port was not recognized: %q", got)
	}
	if got := ansi.Strip(portsText(st, Sandbox{Ports: elsewhere}, map[int]int{7100: 7100})); got != "" {
		t.Fatalf("a desktop on another port was still listed as a port: %q", got)
	}

	plain := []Port{{Number: 6900, Protocol: "http"}}
	if got := desktopText(st, Sandbox{Ports: plain}, map[int]int{6900: 6900}); got != "" {
		t.Fatalf("an undeclared port on 6900 was treated as the desktop: %q", got)
	}
	if got := ansi.Strip(portsText(st, Sandbox{Ports: plain}, map[int]int{6900: 6900})); got != "http:6900" {
		t.Fatalf("an undeclared port on 6900 was not listed: %q", got)
	}
}
