package termpane

import (
	"strings"
	"testing"
)

// A host watching an OSC number is handed each such sequence the far end
// writes, in order, and the emulator still does what it would have: here, a
// title it keeps.
func TestWithOSCWatchesWithoutHandling(t *testing.T) {
	var seen []string
	m, stream, cmd := attach(t, 40, 5,
		WithOSC(7501, func(data []byte) { seen = append(seen, string(data)) }),
		WithOSC(2, func(data []byte) { seen = append(seen, string(data)) }),
	)

	stream.send("\x1b]7501;state=working\x1b\\\x1b]2;the title\a\x1b]7501;state=idle\adone")
	pumpCopies(t, m, cmd, "done")

	if want := "7501;state=working|2;the title|7501;state=idle"; strings.Join(seen, "|") != want {
		t.Fatalf("seen = %q, want %q", strings.Join(seen, "|"), want)
	}
	if m.Title() != "the title" {
		t.Fatalf("title = %q: watching OSC 2 took it from the emulator", m.Title())
	}
}
