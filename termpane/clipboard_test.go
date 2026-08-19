package termpane

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// osc52 is what an application writes to copy text.
func osc52(targets, text string) string {
	return "\x1b]52;" + targets + ";" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"
}

// pumpCopies runs the pane's commands the way the runtime would, until the
// screen shows want, and returns the text of every [CopyMsg] produced on the
// way. Output and copies come back from the same Update as a batch, so the
// batch is walked rather than fed back in.
func pumpCopies(t *testing.T, m *Model, cmd tea.Cmd, want string) []string {
	t.Helper()
	var copies []string
	queue := []tea.Cmd{cmd}
	deadline := time.Now().Add(2 * time.Second)
	for len(queue) > 0 && time.Now().Before(deadline) {
		next := queue[0]
		queue = queue[1:]
		if next == nil {
			continue
		}
		msg, ok := runWithin(next, 200*time.Millisecond)
		if !ok {
			// The pump is waiting for output that is not coming. Everything
			// the test sent has been drawn, so drop it and finish the queue.
			if strings.Contains(screen(m), want) {
				continue
			}
			t.Fatalf("waiting for %q; screen:\n%s", want, screen(m))
		}
		switch msg := msg.(type) {
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case CopyMsg:
			copies = append(copies, msg.Text)
		default:
			_, out := m.Update(msg)
			queue = append(queue, out)
		}
	}
	if !strings.Contains(screen(m), want) {
		t.Fatalf("timed out waiting for %q; screen:\n%s", want, screen(m))
	}
	return copies
}

// An application that copies gets its copy handed to the host, the same way a
// mouse selection does. Without this the sequence is parsed and dropped, and
// yanking in vim over a remote terminal silently does nothing.
func TestApplicationCopyReachesTheHost(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)

	stream.send(osc52("c", "yanked from the box") + "done")
	copies := pumpCopies(t, m, cmd, "done")

	if len(copies) != 1 || copies[0] != "yanked from the box" {
		t.Fatalf("got %q, want one copy of %q", copies, "yanked from the box")
	}
	// The sequence is the pane's; none of it belongs on the screen.
	if got := screen(m); strings.Contains(got, "52") || strings.Contains(got, ";") {
		t.Fatalf("the copy sequence was drawn:\n%s", got)
	}
}

// The last copy in a burst is the one the clipboard would have ended up
// holding, so it is the one delivered.
func TestLastCopyInABurstWins(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)

	stream.send(osc52("c", "first") + osc52("c", "second") + "done")
	copies := pumpCopies(t, m, cmd, "done")

	if len(copies) != 1 || copies[0] != "second" {
		t.Fatalf("got %q, want one copy of %q", copies, "second")
	}
}

// Reading the clipboard is not answered: anything that can scroll text past the
// terminal could otherwise collect whatever the user last copied.
func TestClipboardReadIsNotAnswered(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)

	stream.send("\x1b]52;c;?\x07" + "done")
	if copies := pumpCopies(t, m, cmd, "done"); len(copies) != 0 {
		t.Fatalf("a read produced copies: %q", copies)
	}
	if got := stream.sent(t, ""); strings.Contains(got, "52") {
		t.Fatalf("the pane answered a clipboard read: %q", got)
	}
}

func TestParseClipboardCopy(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("text"))
	for _, tc := range []struct {
		name string
		data string
		want string
		ok   bool
	}{
		{"clipboard", "52;c;" + encoded, "text", true},
		{"primary", "52;p;" + encoded, "text", true},
		{"configured selection", "52;s;" + encoded, "text", true},
		{"default selection", "52;;" + encoded, "text", true},
		{"several targets", "52;pc;" + encoded, "text", true},
		{"a target among cut buffers", "52;01c;" + encoded, "text", true},
		{"cut buffers only", "52;01;" + encoded, "", false},
		{"a read", "52;c;?", "", false},
		{"a clear", "52;c;", "", false},
		{"not base64", "52;c;not base64!", "", false},
		{"no payload field", "52;c", "", false},
		{"multi-line text", "52;c;" + base64.StdEncoding.EncodeToString([]byte("a\nb")), "a\nb", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseClipboardCopy([]byte(tc.data))
			if ok != tc.ok || got != tc.want {
				t.Fatalf("got (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
