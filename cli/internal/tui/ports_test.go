package tui

import (
	"testing"

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
			if got := ansi.Strip(portsText(st, Sandbox{Ports: tc.ports})); got != tc.want {
				t.Fatalf("portsText = %q, want %q", got, tc.want)
			}
		})
	}
}
