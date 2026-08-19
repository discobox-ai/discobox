package termpane

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// fakeStream is a terminal on the end of two pipes: what the test writes shows
// up on the pane's screen, and what the pane sends shows up in sent().
type fakeStream struct {
	out    chan []byte
	closed chan struct{}
	once   sync.Once

	mu      sync.Mutex
	written []byte
	sizes   [][2]int
	err     error
}

func newFakeStream() *fakeStream {
	return &fakeStream{out: make(chan []byte, 16), closed: make(chan struct{})}
}

func (f *fakeStream) Read(p []byte) (int, error) {
	select {
	case chunk, ok := <-f.out:
		if !ok {
			return 0, io.EOF
		}
		return copy(p, chunk), nil
	case <-f.closed:
		return 0, io.EOF
	}
}

func (f *fakeStream) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.written = append(f.written, p...)
	return len(p), nil
}

func (f *fakeStream) Resize(cols, rows int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sizes = append(f.sizes, [2]int{cols, rows})
	return nil
}

func (f *fakeStream) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

// send makes the far end print something.
func (f *fakeStream) send(s string) { f.out <- []byte(s) }

// sent is everything the pane has written to the far end. It is polled because
// input travels through the emulator on its own goroutine.
func (f *fakeStream) sent(t *testing.T, want string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		got := string(f.written)
		f.mu.Unlock()
		if want == "" || strings.Contains(got, want) || time.Now().After(deadline) {
			return got
		}
		time.Sleep(time.Millisecond)
	}
}

// sentN is sent for a substring expected more than once: it waits for the nth
// occurrence. Input reaches the far end through a goroutine of its own, so a
// test that goes on to clear what has been written has to wait for all of it,
// not just the first — the rest would land afterwards and read as new.
func (f *fakeStream) sentN(t *testing.T, want string, n int) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		got := string(f.written)
		f.mu.Unlock()
		if strings.Count(got, want) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *fakeStream) resizes() [][2]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]int(nil), f.sizes...)
}

// attach builds a sized, attached pane and hands back the command that pumps
// its output.
//
// The command is returned rather than run here because running one abandons a
// goroutine blocked on the output channel, and a second consumer would race it
// for the next chunk. Exactly one pump is in flight at a time, and the tests
// thread it through.
func attach(t *testing.T, cols, rows int, opts ...Option) (*Model, *fakeStream, tea.Cmd) {
	t.Helper()
	m := New(opts...)
	m.SetSize(cols, rows)
	stream := newFakeStream()
	cmd := m.Attach(stream)
	t.Cleanup(func() { _ = m.Close() })
	if cmd == nil {
		t.Fatal("Attach returned no command to pump the stream")
	}
	return m, stream, cmd
}

// pump runs the pane's own commands the way the runtime would, until the screen
// contains want, and returns the next command so the caller can carry on.
//
// Everything a test waits for is waited for through the screen, including the
// things that never appear on it: send the escape sequence with a marker behind
// it, and the marker landing proves the sequence was processed.
func pump(t *testing.T, m *Model, cmd tea.Cmd, want string) tea.Cmd {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for cmd != nil && time.Now().Before(deadline) {
		if strings.Contains(screen(m), want) {
			return cmd
		}
		msg, ok := runWithin(cmd, 200*time.Millisecond)
		if !ok {
			t.Fatalf("waiting for %q; screen:\n%s", want, screen(m))
		}
		_, cmd = m.Update(msg)
	}
	if !strings.Contains(screen(m), want) {
		t.Fatalf("timed out waiting for %q; screen:\n%s", want, screen(m))
	}
	return cmd
}

func runWithin(cmd tea.Cmd, d time.Duration) (tea.Msg, bool) {
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		return msg, true
	case <-time.After(d):
		return nil, false
	}
}

// screen is the pane's rendered grid as plain text.
func screen(m *Model) string {
	rows := m.View()
	for i, row := range rows {
		rows[i] = strings.TrimRight(ansi.Strip(row), " ")
	}
	return strings.Join(rows, "\n")
}

// What the far end prints is what the pane draws.
func TestOutputReachesTheScreen(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)

	stream.send("hello")
	pump(t, m, cmd, "hello")
}

// The grid is exactly the size it was given, whatever is on it: a row that came
// out short would shift everything drawn beside it, and one that came out long
// would wrap and shift everything below.
func TestViewIsExactlyTheSizeItWasGiven(t *testing.T) {
	m, stream, cmd := attach(t, 20, 4)
	stream.send("a line that is far longer than twenty cells\r\nshort")
	pump(t, m, cmd, "short")

	rows := m.View()
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4", len(rows))
	}
	for i, row := range rows {
		if w := ansi.StringWidth(row); w != 20 {
			t.Errorf("row %d is %d cells, want 20: %q", i, w, ansi.Strip(row))
		}
	}
}

// A pane resizes both halves: the screen it keeps, and the terminal the far end
// thinks it is drawing on.
func TestResizeTellsBothEnds(t *testing.T) {
	m, stream, _ := attach(t, 40, 10)

	m.SetSize(60, 20)
	if cols, rows := m.Size(); cols != 60 || rows != 20 {
		t.Fatalf("size = %dx%d", cols, rows)
	}
	if got := len(m.View()); got != 20 {
		t.Fatalf("view has %d rows, want 20", got)
	}
	sizes := stream.resizes()
	if len(sizes) < 2 || sizes[len(sizes)-1] != [2]int{60, 20} {
		t.Fatalf("resizes = %v, want the last to be 60x20", sizes)
	}

	// The same size again is not a resize: it would be a round trip to tell the
	// far end something it already knows, on every frame that recomputes layout.
	before := len(stream.resizes())
	m.SetSize(60, 20)
	if after := len(stream.resizes()); after != before {
		t.Fatalf("resizing to the same size told the far end again")
	}
}

// Typing reaches the far end. Printable text goes as text — routed as a key it
// would arrive unshifted, so "A" would arrive as "a".
func TestKeysReachTheFarEnd(t *testing.T) {
	m, stream, _ := attach(t, 40, 5)

	m.Update(key("A"))
	if got := stream.sent(t, "A"); !strings.Contains(got, "A") {
		t.Fatalf("sent %q, want an uppercase A", got)
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := stream.sent(t, "\r"); !strings.Contains(got, "\r") {
		t.Fatalf("sent %q, want a carriage return for enter", got)
	}

	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if got := stream.sent(t, "\x03"); !strings.Contains(got, "\x03") {
		t.Fatalf("sent %q, want ETX for ctrl+c", got)
	}
}

// Paste goes through the emulator so the application's own bracketed-paste mode
// decides how it arrives.
func TestPasteReachesTheFarEnd(t *testing.T) {
	m, stream, _ := attach(t, 40, 5)

	m.Update(tea.PasteMsg{Content: "pasted"})
	if got := stream.sent(t, "pasted"); !strings.Contains(got, "pasted") {
		t.Fatalf("sent %q, want the pasted text", got)
	}
}

// An application that has asked for bracketed paste gets its markers, and one
// that has not gets the text bare.
func TestPasteIsBracketedOnlyWhenAsked(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)

	m.Update(tea.PasteMsg{Content: "plain"})
	if got := stream.sent(t, "plain"); strings.Contains(got, "\x1b[200~") {
		t.Fatalf("sent %q, want no markers before the application asked", got)
	}

	stream.send("\x1b[?2004h" + "asked")
	pump(t, m, cmd, "asked")

	// Waited on by the closing marker, which is the last thing the pane writes.
	m.Update(tea.PasteMsg{Content: "bracketed"})
	if got := stream.sent(t, "\x1b[201~"); !strings.Contains(got, "\x1b[200~bracketed\x1b[201~") {
		t.Fatalf("sent %q, want the paste bracketed", got)
	}
}

// A paste carrying the end marker cannot close its own paste: everything after
// it would arrive as though it had been typed.
func TestPasteMarkersAreStrippedFromTheText(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send("\x1b[?2004h" + "asked")
	pump(t, m, cmd, "asked")

	m.Update(tea.PasteMsg{Content: "safe\x1b[201~rm -rf /\rmore\x1b[200~"})
	// Waiting on the pane's own closing marker, which is the last thing it
	// writes: waiting on the text would race the end of the paste.
	got := stream.sent(t, "\x1b[201~")
	if want := "\x1b[200~saferm -rf /\rmore\x1b[201~"; got != want {
		t.Fatalf("sent %q, want %q", got, want)
	}
}

// A far end that stops accepting input must not take the host down with it. The
// emulator's input pipe is synchronous, and everything that writes to it — a
// paste, a key, the emulator's own replies to the far end's queries — runs on
// the update goroutine, so a forwarder that gave up would block the window
// forever on the next keystroke.
func TestInputSurvivesAStreamThatFails(t *testing.T) {
	m, stream, _ := attach(t, 40, 5)

	stream.mu.Lock()
	stream.err = errors.New("the far end is gone")
	stream.mu.Unlock()

	// The first send is the one that fails the write and ends the forwarding.
	m.Update(tea.PasteMsg{Content: "first"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Enough to fill anything holding what cannot be sent.
		for range 200 {
			m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
		}
		m.Update(tea.PasteMsg{Content: strings.Repeat("x", 4*inputChunk)})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("input blocked the host after the stream failed")
	}
}

// A paste is handed off rather than written through: the host goroutine must
// not wait on a far end that is slow to take it.
func TestALargePasteDoesNotWaitOnTheFarEnd(t *testing.T) {
	m := New()
	m.SetSize(40, 5)
	stream := &slowStream{fakeStream: newFakeStream(), delay: 50 * time.Millisecond}
	if cmd := m.Attach(stream); cmd == nil {
		t.Fatal("Attach returned no command to pump the stream")
	}
	t.Cleanup(func() { _ = m.Close() })

	// Enough chunks that writing them through at the stream's pace would take
	// well over a second — and few enough to fit the backlog, which is the
	// point: the host hands the paste over and carries on.
	start := time.Now()
	m.Update(tea.PasteMsg{Content: strings.Repeat("x", 32*inputChunk)})
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("the paste held the host for %v", took)
	}
}

// slowStream is a far end that takes its time, the way a pty whose program is
// not reading its input does.
type slowStream struct {
	*fakeStream
	delay time.Duration
}

func (s *slowStream) Write(p []byte) (int, error) {
	time.Sleep(s.delay)
	return s.fakeStream.Write(p)
}

// The detach key is a press of its own, and it does not reach the application.
func TestDetachKeyDetaches(t *testing.T) {
	m, stream, _ := attach(t, 40, 5, WithPrefix("ctrl+a", "ctrl+c"))

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("the detach key should emit a message")
	}
	if _, ok := cmd().(DetachMsg); !ok {
		t.Fatalf("got %T, want DetachMsg", cmd())
	}
	if got := stream.sent(t, ""); strings.Contains(got, "\x03") {
		t.Fatalf("the detach key should not also reach the application: %q", got)
	}
}

// The prefix types either reserved key literally, which is how the application
// gets the interrupt the pane has taken.
func TestPrefixTypesTheReservedKeys(t *testing.T) {
	m, stream, _ := attach(t, 40, 5, WithPrefix("ctrl+a", "ctrl+c"))

	// The prefix alone is swallowed: nothing reaches the far end yet.
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	if cmd != nil {
		t.Fatal("the prefix alone should do nothing but arm")
	}
	if got := stream.sent(t, ""); strings.Contains(got, "\x01") {
		t.Fatalf("the prefix should not reach the far end on its own: %q", got)
	}

	// Prefix then the detach key is a real interrupt.
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd != nil {
		t.Fatal("prefix then detach should send the key, not detach")
	}
	if got := stream.sent(t, "\x03"); !strings.Contains(got, "\x03") {
		t.Fatalf("sent %q, want a real interrupt", got)
	}

	// And prefix then prefix is a literal prefix.
	m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	if got := stream.sent(t, "\x01"); !strings.Contains(got, "\x01") {
		t.Fatalf("sent %q, want one literal prefix", got)
	}
}

// Typing the prefix takes it twice in full. Its bare letter is left to
// bindings, because a host with a Ctrl-A leader will want `a` — and a key that
// silently typed a literal prefix instead of running what it was bound to would
// be a key that never worked.
func TestThePrefixesOwnLetterIsAvailableToBindings(t *testing.T) {
	type opened struct{}
	m, stream, _ := attach(t, 40, 5,
		WithPrefix("ctrl+a", "ctrl+c"),
		WithPrefixBinding("a", opened{}))

	m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	_, cmd := m.Update(key("a"))
	if cmd == nil {
		t.Fatal("the binding should have fired")
	}
	if _, ok := cmd().(opened); !ok {
		t.Fatalf("got %T, want the bound message", cmd())
	}
	if got := stream.sent(t, ""); strings.Contains(got, "\x01") {
		t.Fatalf("nothing should have reached the far end: %q", got)
	}
}

// A prefix that qualified nothing is delivered along with the key that followed
// it: a mistyped prefix costs nothing.
func TestMistypedPrefixCostsNothing(t *testing.T) {
	m, stream, _ := attach(t, 40, 5, WithPrefix("ctrl+a", "ctrl+c"))

	m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	m.Update(key("x"))
	if got := stream.sent(t, "x"); !strings.Contains(got, "\x01x") {
		t.Fatalf("sent %q, want the swallowed prefix and then the key", got)
	}
}

// Without a prefix configured, every key belongs to the far end — including the
// ones a host might have wanted.
func TestWithoutAPrefixEveryKeyIsForwarded(t *testing.T) {
	m, stream, _ := attach(t, 40, 5)

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	if cmd != nil {
		t.Fatal("no prefix is configured, so nothing should be intercepted")
	}
	if got := stream.sent(t, "\x01"); !strings.Contains(got, "\x01") {
		t.Fatalf("sent %q, want ctrl+a forwarded", got)
	}
}

// The emulator answers the questions applications ask about the terminal they
// are in. Those replies are input like any other and have to reach the far end,
// or the emulator's buffer fills and the session wedges on the first query.
func TestQueryRepliesReachTheFarEnd(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)

	// Primary device attributes: "what are you?", with a marker behind it so
	// the test can see that it was processed.
	stream.send("\x1b[cMARK")
	pump(t, m, cmd, "MARK")

	if got := stream.sent(t, "\x1b["); !strings.Contains(got, "\x1b[") {
		t.Fatalf("sent %q, want a reply to the device attributes query", got)
	}
}

// A session that ends says so, once, with the reason.
func TestClosedStreamReportsOnce(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)

	stream.Close()
	msg, ok := runWithin(cmd, time.Second)
	if !ok {
		t.Fatal("a closed stream should end the read")
	}
	_, cmd = m.Update(msg)
	if cmd == nil {
		t.Fatal("a closed stream should report")
	}
	closed, ok := cmd().(ClosedMsg)
	if !ok {
		t.Fatalf("got %T, want ClosedMsg", cmd())
	}
	if closed.Err != nil && !errors.Is(closed.Err, io.EOF) {
		t.Fatalf("err = %v, want nil or EOF", closed.Err)
	}
	if m.Attached() {
		t.Fatal("a closed pane is not attached")
	}
}

// The cursor is placed where the application put it, offset by wherever the
// host drew the grid — and withdrawn entirely when the application hides it.
func TestCursorFollowsTheApplication(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)

	stream.send("abc")
	cmd = pump(t, m, cmd, "abc")

	cursor := m.Cursor(10, 4)
	if cursor == nil {
		t.Fatal("a visible cursor should be placed")
	}
	if cursor.X != 13 || cursor.Y != 4 {
		t.Fatalf("cursor at %d,%d, want 13,4 — the origin plus three characters", cursor.X, cursor.Y)
	}

	// DECTCEM off: the application has hidden the cursor.
	stream.send("\x1b[?25lHIDDEN")
	pump(t, m, cmd, "HIDDEN")
	if m.Cursor(10, 4) != nil {
		t.Fatal("a hidden cursor should not be placed")
	}
}

// The alternate screen is how a full-screen application is told from a shell
// prompt, which is worth knowing when deciding what chrome to draw around it.
func TestAltScreenIsReported(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	if m.AltScreen() {
		t.Fatal("a fresh pane is not on the alternate screen")
	}

	stream.send("\x1b[?1049hALT")
	pump(t, m, cmd, "ALT")
	if !m.AltScreen() {
		t.Fatal("entering the alternate screen should be reported")
	}
}

// The title an application sets is worth showing in whatever chrome the host
// draws, the way a terminal shows it in its own title bar.
func TestTitleIsReported(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)

	stream.send("\x1b]2;building\x07TITLED")
	pump(t, m, cmd, "TITLED")
	if got := m.Title(); got != "building" {
		t.Fatalf("title = %q, want building", got)
	}
}

// A burst of writes is one screen the user is waiting to see, so it arrives as
// one message rather than one render per chunk.
func TestOutputBurstsAreCoalesced(t *testing.T) {
	_, stream, cmd := attach(t, 40, 5)

	for _, chunk := range []string{"one ", "two ", "three"} {
		stream.send(chunk)
	}
	// Give the reader a moment to queue all three behind one another.
	time.Sleep(50 * time.Millisecond)

	msg, ok := runWithin(cmd, time.Second)
	if !ok {
		t.Fatal("expected output")
	}
	out, ok := msg.(outputMsg)
	if !ok {
		t.Fatalf("got %T, want outputMsg", msg)
	}
	if got := string(out.data); got != "one two three" {
		t.Fatalf("coalesced into %q, want the whole burst in one message", got)
	}
}

// Closing is safe to do twice, and safe on a pane that never attached.
func TestCloseIsIdempotent(t *testing.T) {
	if err := New().Close(); err != nil {
		t.Fatalf("closing an unattached pane: %v", err)
	}
	m, _, _ := attach(t, 40, 5)
	if err := m.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if m.Attached() {
		t.Fatal("a closed pane is not attached")
	}
}

// Attaching a second stream replaces the first rather than leaking it.
func TestReattachReplacesTheStream(t *testing.T) {
	m, first, _ := attach(t, 40, 5)

	second := newFakeStream()
	cmd := m.Attach(second)
	t.Cleanup(func() { _ = m.Close() })
	if cmd == nil {
		t.Fatal("reattach should return a pump")
	}

	select {
	case <-first.closed:
	case <-time.After(time.Second):
		t.Fatal("the replaced stream should have been closed")
	}

	second.send("second")
	pump(t, m, cmd, "second")
	if !strings.Contains(screen(m), "second") {
		t.Fatalf("screen:\n%s", screen(m))
	}
}

func key(s string) tea.KeyPressMsg {
	runes := []rune(s)
	return tea.KeyPressMsg{Code: runes[0], Text: s}
}

// The application asking for the mouse is read off the stream, because the
// emulator tracks it but will not say. A host mirrors it so the terminal only
// reports the mouse while something is using it.
func TestMouseModeFollowsTheApplication(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	if m.MouseMode() != MouseNone {
		t.Fatal("nothing has asked for the mouse yet")
	}

	stream.send("\x1b[?1000h\x1b[?1006hCLICKS")
	cmd = pump(t, m, cmd, "CLICKS")
	if got := m.MouseMode(); got != MouseCellMotion {
		t.Fatalf("mode = %v, want cell motion", got)
	}

	stream.send("\x1b[?1003hMOTION")
	cmd = pump(t, m, cmd, "MOTION")
	if got := m.MouseMode(); got != MouseAllMotion {
		t.Fatalf("mode = %v, want all motion", got)
	}

	stream.send("\x1b[?1000l\x1b[?1003lOFF")
	pump(t, m, cmd, "OFF")
	if got := m.MouseMode(); got != MouseNone {
		t.Fatalf("mode = %v, want none once the application gives it back", got)
	}
}

// Observing the mode must not consume the sequence: the emulator still has to
// apply it, or the events forwarded next would be dropped as unasked for.
func TestWatchingMouseModesDoesNotConsumeThem(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send("\x1b[?1000h\x1b[?1006hREADY")
	pump(t, m, cmd, "READY")

	m.SendMouse(tea.MouseClickMsg{X: 2, Y: 1, Button: tea.MouseLeft})
	if got := stream.sent(t, "\x1b[<"); !strings.Contains(got, "\x1b[<") {
		t.Fatalf("sent %q, want an SGR-encoded mouse report", got)
	}
}

// An event the application never asked for is dropped, so a host may forward
// unconditionally; and a click on the host's own chrome is not the
// application's to hear about.
func TestMouseEventsOutsideTheGridAndModeAreDropped(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)

	// No mode set yet: the emulator drops it.
	m.SendMouse(tea.MouseClickMsg{X: 2, Y: 1, Button: tea.MouseLeft})
	if got := stream.sent(t, ""); strings.Contains(got, "\x1b[<") || strings.Contains(got, "\x1b[M") {
		t.Fatalf("sent %q, want nothing before the application asks", got)
	}

	stream.send("\x1b[?1000h\x1b[?1006hREADY")
	pump(t, m, cmd, "READY")

	// Outside the grid: dropped here.
	m.SendMouse(tea.MouseClickMsg{X: 99, Y: 99, Button: tea.MouseLeft})
	if got := stream.sent(t, ""); strings.Contains(got, "\x1b[<") {
		t.Fatalf("sent %q, want nothing for a click outside the grid", got)
	}
}

// A host's own prefix bindings emit their message instead of reaching the
// application.
func TestPrefixBindingsEmitTheirMessage(t *testing.T) {
	type toggle struct{}
	m, stream, _ := attach(t, 40, 5, WithPrefix("ctrl+a", "ctrl+c"), WithPrefixBinding("m", toggle{}))

	m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	_, cmd := m.Update(key("m"))
	if cmd == nil {
		t.Fatal("a bound key should emit its message")
	}
	if _, ok := cmd().(toggle); !ok {
		t.Fatalf("got %T, want the bound message", cmd())
	}
	if got := stream.sent(t, ""); strings.Contains(got, "m") {
		t.Fatalf("a bound key should not also reach the application: %q", got)
	}
}

// Holding Ctrl through the pair is what everyone does, so a key typed after the
// prefix matches either way. It applies only after the prefix: the bare letter
// on its own is one the application should get.
func TestAHeldCtrlAfterThePrefixIsTolerated(t *testing.T) {
	type toggle struct{}
	for _, second := range []string{"m", "ctrl+m"} {
		m, _, _ := attach(t, 40, 5, WithPrefix("ctrl+a", "ctrl+c"), WithPrefixBinding("m", toggle{}))
		m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
		_, cmd := m.Update(keyNamed(second))
		if cmd == nil {
			t.Fatalf("%q after the prefix should have been bound", second)
		}
		if _, ok := cmd().(toggle); !ok {
			t.Fatalf("%q: got %T, want the bound message", second, cmd())
		}
	}

	// Either form of the detach key sends a real interrupt.
	for _, second := range []string{"c", "ctrl+c"} {
		m, stream, _ := attach(t, 40, 5, WithPrefix("ctrl+a", "ctrl+c"))
		m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
		m.Update(keyNamed(second))
		if got := stream.sent(t, "\x03"); !strings.Contains(got, "\x03") {
			t.Fatalf("%q after the prefix sent %q, want an interrupt", second, got)
		}
	}

	// And either form of the prefix types one.
	for _, second := range []string{"a", "ctrl+a"} {
		m, stream, _ := attach(t, 40, 5, WithPrefix("ctrl+a", "ctrl+c"))
		m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
		m.Update(keyNamed(second))
		if got := stream.sent(t, "\x01"); !strings.Contains(got, "\x01") {
			t.Fatalf("%q after the prefix sent %q, want a literal prefix", second, got)
		}
	}
}

// The tolerance stops at the prefix: a bare letter typed on its own is the
// application's, and only the detach key exactly as configured detaches.
func TestToleranceDoesNotLeakOutsideThePrefix(t *testing.T) {
	m, stream, _ := attach(t, 40, 5, WithPrefix("ctrl+a", "ctrl+c"))

	_, cmd := m.Update(key("c"))
	if cmd != nil {
		t.Fatal("a bare c should not detach")
	}
	if got := stream.sent(t, "c"); !strings.Contains(got, "c") {
		t.Fatalf("sent %q, want the letter to reach the application", got)
	}
}

// keyNamed builds a key press from a name, for the two forms a held Ctrl gives.
func keyNamed(name string) tea.KeyPressMsg {
	if rest, ok := strings.CutPrefix(name, "ctrl+"); ok {
		return tea.KeyPressMsg{Code: rune(rest[0]), Mod: tea.ModCtrl}
	}
	return key(name)
}

// Special keys held with Ctrl or Shift reach the application.
//
// The emulator does not encode them: it matches key events by exact equality,
// so a Left carrying a modifier matches none of its cases and produces nothing
// at all. Without the encoding in keys.go, Ctrl-Left and its like arrive as
// silence — the key is delivered to this package correctly and dropped here.
func TestModifiedSpecialKeysReachTheApplication(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyPressMsg
		want string
	}{
		// Unmodified keys are the emulator's own, and stay that way.
		{"left", tea.KeyPressMsg{Code: tea.KeyLeft}, "\x1b[D"},
		{"home", tea.KeyPressMsg{Code: tea.KeyHome}, "\x1b[H"},
		{"f5", tea.KeyPressMsg{Code: tea.KeyF5}, "\x1b[15~"},
		// Alt alone is left to it too: an escape prefix is a form readline and
		// its like have always understood.
		{"alt+left", tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModAlt}, "\x1b\x1b[D"},

		// The ones it drops, in xterm's encoding: 1 plus a bit per modifier.
		{"ctrl+left", tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModCtrl}, "\x1b[1;5D"},
		{"ctrl+right", tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModCtrl}, "\x1b[1;5C"},
		{"shift+left", tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModShift}, "\x1b[1;2D"},
		{"ctrl+alt+left", tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModCtrl | tea.ModAlt}, "\x1b[1;7D"},
		{"ctrl+home", tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl}, "\x1b[1;5H"},
		{"ctrl+end", tea.KeyPressMsg{Code: tea.KeyEnd, Mod: tea.ModCtrl}, "\x1b[1;5F"},
		{"ctrl+delete", tea.KeyPressMsg{Code: tea.KeyDelete, Mod: tea.ModCtrl}, "\x1b[3;5~"},
		{"shift+pgup", tea.KeyPressMsg{Code: tea.KeyPgUp, Mod: tea.ModShift}, "\x1b[5;2~"},
		{"ctrl+f5", tea.KeyPressMsg{Code: tea.KeyF5, Mod: tea.ModCtrl}, "\x1b[15;5~"},
		{"shift+f1", tea.KeyPressMsg{Code: tea.KeyF1, Mod: tea.ModShift}, "\x1b[1;2P"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, stream, _ := attach(t, 40, 5)
			m.Update(tc.key)
			if got := stream.sent(t, tc.want); got != tc.want {
				t.Fatalf("sent %q, want %q", got, tc.want)
			}
		})
	}
}

// Shift-Backspace is Backspace. Backspace has no modified form to encode —
// xterm sends DEL either way — and the emulator drops the shifted key, so
// without the fold in keys.go it reaches the application as silence.
func TestShiftBackspaceIsBackspace(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{"backspace", tea.KeyPressMsg{Code: tea.KeyBackspace}},
		{"shift+backspace", tea.KeyPressMsg{Code: tea.KeyBackspace, Mod: tea.ModShift}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, stream, _ := attach(t, 40, 5)
			m.Update(tc.key)
			if got := stream.sent(t, "\x7f"); got != "\x7f" {
				t.Fatalf("sent %q, want DEL", got)
			}
		})
	}
}

// A modified cursor key takes the CSI form whether or not the application has
// asked for application cursor keys: that is what xterm does, and the SS3 form
// the emulator uses for the unmodified key has nowhere to put a modifier.
func TestModifiedCursorKeysIgnoreApplicationMode(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send("\x1b[?1hAPPMODE") // DECCKM on
	pump(t, m, cmd, "APPMODE")

	m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if got := stream.sent(t, "\x1bOD"); got != "\x1bOD" {
		t.Fatalf("unmodified sent %q, want the application form", got)
	}

	m2, stream2, cmd2 := attach(t, 40, 5)
	stream2.send("\x1b[?1hAPPMODE")
	pump(t, m2, cmd2, "APPMODE")
	m2.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModCtrl})
	if got := stream2.sent(t, "\x1b[1;5D"); got != "\x1b[1;5D" {
		t.Fatalf("modified sent %q, want the CSI form", got)
	}
}

// A stream that simply ends reports no error: end of file is the session
// ending, not something going wrong with it, and a host that treated it as a
// failure would report every normal exit as one.
func TestAStreamEndingIsNotAnError(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)

	stream.Close()
	msg, ok := runWithin(cmd, time.Second)
	if !ok {
		t.Fatal("a closed stream should end the read")
	}
	_, next := m.Update(msg)
	if next == nil {
		t.Fatal("a closed stream should report")
	}
	closed, ok := next().(ClosedMsg)
	if !ok {
		t.Fatalf("got %T, want ClosedMsg", next())
	}
	if closed.Err != nil {
		t.Fatalf("err = %v, want nil for a clean end", closed.Err)
	}
}

// Output longer than the pane is kept, and the view can be moved back through
// it: a screen you cannot scroll is a screen whose first half you never read.
func TestScrollbackCanBeLookedAt(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)

	// No newline after the last, so the geometry is exact: the screen holds
	// lines 16 to 20 and the fifteen before them are in the scrollback.
	var sent strings.Builder
	for i := 1; i <= 20; i++ {
		if i > 1 {
			sent.WriteString("\r\n")
		}
		fmt.Fprintf(&sent, "line %d", i)
	}
	stream.send(sent.String())
	pump(t, m, cmd, "line 20")

	// The live screen is the last few lines, and the first are gone from it.
	if screen := screen(m); strings.Contains(screen, "line 1\n") {
		t.Fatalf("line 1 should have scrolled off:\n%s", screen)
	}
	if m.ScrollOffset() != 0 {
		t.Fatalf("offset = %d, want the live screen", m.ScrollOffset())
	}
	if got := m.ScrollbackLen(); got != 15 {
		t.Fatalf("scrollback holds %d lines, want the 15 that scrolled off", got)
	}

	// Back up, and they are there.
	m.Scroll(10)
	if got := m.ScrollOffset(); got != 10 {
		t.Fatalf("offset = %d, want 10", got)
	}
	if !strings.Contains(screen(m), "line 6") {
		t.Fatalf("scrolled back, earlier lines should show:\n%s", screen(m))
	}

	// It stops at the top rather than running into blank rows.
	m.Scroll(1000)
	if got, want := m.ScrollOffset(), m.ScrollbackLen(); got != want {
		t.Fatalf("offset = %d, want it to stop at %d", got, want)
	}
	if !strings.Contains(screen(m), "line 1") {
		t.Fatalf("the top of the history should show:\n%s", screen(m))
	}

	// And back down to the live screen.
	m.Scroll(-1000)
	if m.ScrollOffset() != 0 {
		t.Fatalf("offset = %d, want the live screen", m.ScrollOffset())
	}
	if !strings.Contains(screen(m), "line 20") {
		t.Fatalf("the live screen should show:\n%s", screen(m))
	}
}

// New output pins the view back to the live screen: a pane scrolled away from
// what is happening in it, with no way to notice, is a pane that looks hung.
func TestNewOutputReturnsToTheLiveScreen(t *testing.T) {
	m, stream, cmd := attach(t, 40, 5)
	stream.send(strings.Repeat("filler\r\n", 20))
	cmd = pump(t, m, cmd, "filler")

	m.Scroll(5)
	if m.ScrollOffset() == 0 {
		t.Fatal("expected to be scrolled back")
	}

	stream.send("something new\r\n")
	pump(t, m, cmd, "something new")
	if m.ScrollOffset() != 0 {
		t.Fatalf("offset = %d, want the live screen", m.ScrollOffset())
	}
}

// A repeating binding holds the prefix open while Ctrl is held, so a run of
// them is one chord rather than one prefix per step.
func TestRepeatingBindingsHoldThePrefix(t *testing.T) {
	type moved struct{}
	newPane := func() (*Model, *fakeStream) {
		m, stream, _ := attach(t, 40, 5,
			WithPrefix("ctrl+a", "ctrl+c"),
			WithRepeatingPrefixBinding("l", moved{}),
			WithPrefixBinding("s", struct{}{}))
		return m, stream
	}

	// Prefix, then a run of ctrl-held keys: every one fires.
	m, _ := newPane()
	m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	for i := range 3 {
		_, cmd := m.Update(tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl})
		if cmd == nil {
			t.Fatalf("press %d did not fire", i+1)
		}
		if _, ok := cmd().(moved); !ok {
			t.Fatalf("press %d: got %T, want the bound message", i+1, cmd())
		}
	}

	// Letting go of Ctrl fires once and closes the sequence: the next key is
	// the application's again.
	m, stream := newPane()
	m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	if _, cmd := m.Update(key("l")); cmd == nil {
		t.Fatal("the bare key should still fire once")
	}
	if _, cmd := m.Update(key("l")); cmd != nil {
		t.Fatal("the sequence should have closed")
	}
	if got := stream.sent(t, "l"); !strings.Contains(got, "l") {
		t.Fatalf("sent %q, want the second key to reach the application", got)
	}

	// A binding that does not repeat closes the sequence even held with Ctrl.
	m, _ = newPane()
	m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}); cmd == nil {
		t.Fatal("the binding should fire")
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}); cmd != nil {
		t.Fatal("a binding that does not repeat should close the sequence")
	}
}

// Mid-run the open prefix belongs to the run, not to the binding table: after
// prefix ctrl-l ctrl-l, letting go of Ctrl and typing a bound letter must type
// it, not fire the binding — the prefix was held open for the run, never
// re-pressed.
func TestARunTakesOnlyItsOwnKeys(t *testing.T) {
	type moved struct{}
	type acted struct{}
	newRun := func() (*Model, *fakeStream) {
		m, stream, _ := attach(t, 40, 5,
			WithPrefix("ctrl+a", "ctrl+c"),
			WithRepeatingPrefixBinding("l", moved{}),
			WithPrefixBinding("x", acted{}))
		m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
		if _, cmd := m.Update(tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl}); cmd == nil {
			t.Fatal("the run should have started")
		}
		return m, stream
	}

	// A bound letter after the run is the application's first keystroke.
	m, stream := newRun()
	if _, cmd := m.Update(key("x")); cmd != nil {
		t.Fatalf("got %T, want the bound letter to end the run unfired", cmd())
	}
	if got := stream.sent(t, "x"); !strings.Contains(got, "x") {
		t.Fatalf("sent %q, want the letter delivered to the application", got)
	}

	// So is the repeating key itself once Ctrl is let go.
	m, stream = newRun()
	if _, cmd := m.Update(key("l")); cmd != nil {
		t.Fatal("a bare repeating key should end the run, not fire")
	}
	if got := stream.sent(t, "l"); !strings.Contains(got, "l") {
		t.Fatalf("sent %q, want the key delivered to the application", got)
	}

	// While Ctrl is held, even a non-repeating bound key ends the run and is
	// delivered: the run's vocabulary is the repeating keys and nothing else.
	m, stream = newRun()
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}); cmd != nil {
		t.Fatal("a non-repeating binding must not fire mid-run")
	}
	if got := stream.sent(t, "\x18"); !strings.Contains(got, "\x18") {
		t.Fatalf("sent %q, want ctrl+x delivered to the application", got)
	}

	// And a carried run — armed by the host on the pane focus moved to — is
	// the same run: the repeating key continues it, a letter ends it.
	m, stream = newRun()
	m.SetPrefixArmed(false)
	m.SetPrefixArmed(true) // what a host does to the pane it moves onto
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl}); cmd == nil {
		t.Fatal("the carried run should continue under Ctrl")
	}
	if _, cmd := m.Update(key("x")); cmd != nil {
		t.Fatal("a letter should end the carried run unfired")
	}
	if got := stream.sent(t, "x"); !strings.Contains(got, "x") {
		t.Fatalf("sent %q, want the letter delivered to the application", got)
	}
}

// The title belongs to the stream. Reattaching starts with none, and takes
// whatever the new far end announces — which, for a session that set its title
// long before this client arrived, is what its repaint snapshot replays.
func TestReattachTakesTheNewStreamsTitle(t *testing.T) {
	m, first, cmd := attach(t, 40, 5)
	first.send("\x1b]2;first\x07ONE")
	pump(t, m, cmd, "ONE")
	if got := m.Title(); got != "first" {
		t.Fatalf("title = %q, want the first stream's", got)
	}

	second := newFakeStream()
	cmd = m.Attach(second)
	t.Cleanup(func() { _ = m.Close() })
	if got := m.Title(); got != "" {
		t.Fatalf("title = %q, want the old stream's forgotten", got)
	}

	second.send("\x1b]2;second\x07TWO")
	pump(t, m, cmd, "TWO")
	if got := m.Title(); got != "second" {
		t.Fatalf("title = %q, want the new stream's", got)
	}
}
