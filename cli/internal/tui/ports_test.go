package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
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

// A forwarded port shows both numbers, so the header says what to type here as
// well as what the sandbox is serving there.
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
	want := "http:3000->3000,8082->8080 · tcp:5433->5432"
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
