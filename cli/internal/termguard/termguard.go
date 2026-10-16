// Package termguard puts a terminal back the way it was found when the process
// that took it over dies without doing so itself.
//
// A full-screen program leaves the terminal in raw mode, on the alternate
// screen, and reporting the mouse, and only the program can undo that. Bubble
// Tea undoes it on every exit it sees — a quit, a signal it handles, a panic in
// Update, View or a command — but a process can end without Bubble Tea seeing
// it: a panic in any other goroutine (its own renderer and input reader
// included), a runtime fatal error such as a concurrent map write, os.Exit, or
// SIGKILL. What that leaves behind is the last frame, no echo or a shell that
// has adopted raw mode, and a mouse whose every movement is typed at the
// prompt as an escape sequence.
//
// Nothing inside the process can cover all of those, so the guard is a second
// process: the same binary, re-run as a hidden command, holding the read end of
// a pipe whose write end only the program holds. The kernel closes that pipe
// however the program ends, so the guard always hears about it. A program that
// finished properly writes one byte first; a pipe that closes without it is a
// program that died, and the guard restores the terminal and says why.
//
// Why is the hard part, since the guard never sees the program die. A runtime
// crash leaves a report (below). The one signal that ends a Go program
// silently and can be caught is SIGHUP — the runtime ignores the other
// catchable ones, traces the rest into the report, and Bubble Tea handles
// SIGINT and SIGTERM itself — so the program catches it, writes its name down
// the lifeline, and dies of it as it would have. On Linux the guard also
// watches its cgroup's count of the kernel out-of-memory killer's kills, and
// blames it for a death that comes with a rise. Where that count is there to
// read, what is left is a SIGKILL from another process or an exit that skipped
// Release, and the guard says so and how to catch the sender; where it is not,
// the guard can only say the console was killed without warning. A kill that
// takes the whole cgroup (systemd-oomd, memory.oom.group) takes the guard too,
// and nothing is said.
//
// The program also points the runtime's crash output (debug.SetCrashOutput) at
// a temporary file. A trace printed to the alternate screen is gone the moment
// the screen is switched back, so the file is where it survives; the guard
// names it in one line rather than reprinting it, since a trace of every
// goroutine runs to a thousand lines and would scroll everything else away.
package termguard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/programstatus"
	"golang.org/x/term"
)

// oomPollInterval is how often the guard reads the out-of-memory kill count
// while the program runs. The out-of-memory killer is blamed only for a rise
// in the last two of them, so this is about how close to the death a kill
// must be.
var oomPollInterval = time.Second

// readyTimeout bounds how long the program waits for the guard to have taken
// the terminal's state. A guard that has not started by then is not one worth
// holding the window for: the program runs unguarded, as it did before there
// was a guard.
const readyTimeout = 5 * time.Second

// ready is the line the guard writes once it holds the terminal's state, and
// finished the byte the program writes before it closes the lifeline on a
// proper exit. Anything else the program writes there is its last word: the
// name of the signal it is about to die of.
const (
	ready    = "ready"
	finished = 'x'
)

// Guard is a running guard, as seen from the program it guards.
type Guard struct {
	cmd      *exec.Cmd
	lifeline io.WriteCloser
	report   *os.File

	// stopHangup undoes watchHangup.
	stopHangup func()

	// mu guards spoken: the program says one thing down the lifeline, that it
	// finished or what it is dying of, and whichever comes first is the one.
	mu     sync.Mutex
	spoken bool
}

// Start runs the guard as command, which must end up in Serve with the report
// path appended to its arguments, and returns once the guard holds the
// terminal's state — so it must be called before the program changes it.
//
// From here until Release, a crash of this process writes its report to a
// temporary file, which Release removes again, and a SIGHUP is caught long
// enough to tell the guard before the process dies of it.
//
// An error means there is no guard, and nothing has been changed: the caller
// runs without one.
func Start(command []string) (*Guard, error) {
	report, err := os.CreateTemp("", "discobox-console-crash-*.log")
	if err != nil {
		return nil, err
	}
	g, err := start(command, report)
	if err != nil {
		_ = report.Close()
		_ = os.Remove(report.Name())
		return nil, err
	}
	if err := debug.SetCrashOutput(report, debug.CrashOptions{}); err != nil {
		g.stop()
		_ = report.Close()
		_ = os.Remove(report.Name())
		return nil, fmt.Errorf("send crash output to %s: %w", report.Name(), err)
	}
	// Every goroutine, not only the one that failed: a race or a deadlock is
	// explained by what the others were doing. It only ever raises the level,
	// so GOTRACEBACK set higher still wins.
	debug.SetTraceback("all")
	g.stopHangup = watchHangup(g.say)
	return g, nil
}

func start(command []string, report *os.File) (*Guard, error) {
	// No context of the console's: the guard is what has to outlive whatever
	// ends the console, cancellation included. Release is what ends it.
	//nolint:gosec // the program is this one and the arguments are its own
	cmd := exec.CommandContext(context.Background(), command[0], append(slices.Clone(command[1:]), report.Name())...)
	lifeline, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the terminal guard: %w", err)
	}
	g := &Guard{cmd: cmd, lifeline: lifeline, report: report}

	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(out).ReadString('\n')
		line <- strings.TrimSpace(s)
	}()
	select {
	case s := <-line:
		if s == ready {
			return g, nil
		}
		g.stop()
		return nil, errors.New("the terminal guard could not take the terminal's state")
	case <-time.After(readyTimeout):
		g.stop()
		return nil, errors.New("the terminal guard did not start")
	}
}

// Release tells the guard the program ended properly and waits for it to go.
// Call it once the terminal has been handed back; the crash report is removed,
// since there is nothing in it.
func (g *Guard) Release() {
	if g == nil {
		return
	}
	g.stopHangup()
	_ = debug.SetCrashOutput(nil, debug.CrashOptions{})
	g.say(string(finished))
	_ = g.lifeline.Close()
	_ = g.cmd.Wait()
	_ = g.report.Close()
	_ = os.Remove(g.report.Name())
}

// say writes word down the lifeline, unless the program has already said
// something there.
func (g *Guard) say(word string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.spoken {
		return
	}
	g.spoken = true
	_, _ = io.WriteString(g.lifeline, word)
}

// stop ends a guard that never became one.
func (g *Guard) stop() {
	_ = g.lifeline.Close()
	_ = g.cmd.Process.Kill()
	_ = g.cmd.Wait()
}

// Serve is the guard: it takes the terminal's state, says so on ready, and
// waits on lifeline. A lifeline that ends without the finished byte is a
// program that died holding the terminal; Serve puts the terminal back and
// says what it can of why: where the report at reportPath is, if the runtime
// wrote one, or else the program's last word, or the out-of-memory killer.
func Serve(lifeline io.Reader, readyOut io.Writer, reportPath string) error {
	ignoreSignals()
	tty, err := openTerminal()
	if err != nil {
		return err
	}
	defer tty.close()
	// The program started the guard, so until it dies it is the parent.
	return serve(tty, lifeline, readyOut, reportPath, os.Getppid(), oomKills)
}

// serve is Serve on a terminal already open, guarding the program with pid
// console, and counting the out-of-memory killer's kills with oomKills.
func serve(tty terminal, lifeline io.Reader, readyOut io.Writer, reportPath string, console int, oomKills func() (string, uint64, bool)) error {
	states, err := tty.save()
	if err != nil {
		return err
	}
	// Counted before the program can be killed, from the cgroup the guard
	// shares with it as its child.
	cgroup, kills, counted := oomKills()
	if _, err := fmt.Fprintln(readyOut, ready); err != nil {
		return err
	}

	said := make(chan []byte, 1)
	go func() { said <- lastWord(lifeline) }()
	// Read again while the program runs, so a kill blamed on the out-of-memory
	// killer is one counted within a poll of the death: the count is the whole
	// cgroup's, which a tmux server or an ssh session shares with every other
	// pane, and over the console's lifetime one of those is bound to be killed.
	var poll <-chan time.Time
	if counted {
		t := time.NewTicker(oomPollInterval)
		defer t.Stop()
		poll = t.C
	}
	// The baseline is the read before the latest, not the latest: the kernel
	// counts a kill as it picks the victim, and the lifeline only closes once
	// the victim has let go of its memory and files, so the latest read may
	// already hold the console's own kill.
	older, latest := kills, kills
	var word []byte
	for word == nil {
		select {
		case word = <-said:
		case <-poll:
			if _, n, ok := oomKills(); ok {
				older, latest = latest, n
			}
		}
	}
	if string(word) == string(finished) {
		return nil
	}
	d := death{console: console, word: string(word), oomCounted: counted}
	if counted {
		if _, n, ok := oomKills(); ok && n > older {
			d.oomCgroup = cgroup
		}
	}

	// The escape sequences first, while the output still interprets them the
	// way the program left it; the modes after. The message goes in the same
	// write: the shell takes the terminal back as the program dies and prints
	// its prompt as soon as it has, and one write is the least there is for
	// that prompt to land in the middle of.
	_, _ = io.WriteString(tty.out, resetSequence+explanation(reportPath, d))
	tty.restore(states)
	return nil
}

// lastWord reads what the program said down the lifeline before it closed:
// finished, the signal it died of, or nothing at all. It is never nil.
func lastWord(lifeline io.Reader) []byte {
	var b [1]byte
	n, _ := io.ReadFull(lifeline, b[:])
	if n == 1 && b[0] == finished {
		// Not read to the end: a program that finished is done with the guard,
		// whatever else is holding the pipe open.
		return b[:]
	}
	rest, _ := io.ReadAll(lifeline)
	return append(b[:n], rest...)
}

// resetSequence undoes everything a Bubble Tea program, and the renderer under
// it, may have left switched on. It is every mode rather than the ones this
// particular frame used, since the guard never saw the frame — and turning off
// a mode that was never on is harmless.
var resetSequence = strings.Join([]string{
	// A frame the program died in the middle of: until this, a terminal that
	// honors synchronized output shows nothing new at all.
	ansi.ResetModeSynchronizedOutput,
	ansi.ResetModeMouseNormal,
	ansi.ResetModeMouseHighlight,
	ansi.ResetModeMouseButtonEvent,
	ansi.ResetModeMouseAnyEvent,
	ansi.ResetModeMouseExtSgr,
	ansi.ResetModeMouseExtUrxvt,
	ansi.ResetModeMouseExtSgrPixel,
	ansi.ResetModeFocusEvent,
	ansi.ResetModeBracketedPaste,
	ansi.ResetModifyOtherKeys,
	ansi.KittyKeyboard(0, 1),
	ansi.ResetModeUnicodeCore,
	ansi.ResetModeInsertReplace,
	ansi.ResetModeOrigin,
	ansi.SetModeAutoWrap,
	ansi.ResetModeCursorKeys,
	ansi.ResetModeNumericKeypad,
	ansi.ResetStyle,
	ansi.ResetProgressBar,
	// The focused discobox's status the console re-emitted (ADR 26-10-16-424
	// §6): a record the window leaves behind would go on claiming that box
	// was working long after the window that knew it was gone. A terminal
	// that never knew the protocol ignores it.
	programstatus.ClearSequence,
	ansi.ResetCursorColor,
	ansi.ResetForegroundColor,
	ansi.ResetBackgroundColor,
	ansi.SetCursorStyle(0),
	ansi.SetWindowTitle(""),
	// The scroll region is the terminal's, not the screen's, so one the
	// renderer set would follow the user back to the primary screen. Resetting
	// it homes the cursor, which is why it is done on the alternate screen,
	// before leaving it restores the primary screen's cursor.
	ansi.SetTopBottomMargins(0, 0),
	ansi.ResetModeAltScreenSaveCursor,
	ansi.ShowCursor,
}, "")

// death is what the guard knows of how the program ended, beyond the crash
// report.
type death struct {
	// console is the program's pid, to match against a kernel log.
	console int
	// word is what the program said down the lifeline as it died: the signal
	// it died of.
	word string
	// oomCounted is whether the out-of-memory kill count could be read, and
	// so whether the out-of-memory killer can be ruled out.
	oomCounted bool
	// oomCgroup is the cgroup the out-of-memory killer killed in as the
	// program died, if it did.
	oomCgroup string
}

// explanation is what the guard says once the terminal is back: the most
// specific reason it has, in one line, and where the report is when the
// runtime wrote one. The written \r\n is because the terminal is still in the
// mode the program left it in when this is written.
func explanation(reportPath string, d death) string {
	if info, err := os.Stat(reportPath); err == nil && info.Size() > 0 {
		return fmt.Sprintf("\r\ndiscobox: the console crashed; the terminal has been restored. The crash report is in %s\r\n", reportPath)
	}
	_ = os.Remove(reportPath)
	switch {
	case d.word != "":
		return fmt.Sprintf("\r\ndiscobox: the console (pid %d) was ended by %s; the terminal has been restored.\r\n", d.console, d.word)
	case d.oomCgroup != "":
		return fmt.Sprintf("\r\ndiscobox: the console (pid %d) was most likely killed by the kernel's out-of-memory killer, which killed a process in its cgroup (%s) while it ran; the terminal has been restored.\r\n", d.console, d.oomCgroup)
	case d.oomCounted:
		return fmt.Sprintf("\r\ndiscobox: the console (pid %d) was killed without warning, by a SIGKILL from another process or an exit that skipped its own cleanup; the terminal has been restored.\r\n%s", d.console, killSenderHint)
	default:
		return fmt.Sprintf("\r\ndiscobox: the console (pid %d) was killed without warning: by the system, perhaps for memory, by another process, or by an exit that skipped its own cleanup; the terminal has been restored.\r\n", d.console)
	}
}

// terminal is the guard's own handle on the terminal the program is drawing on.
// It is opened by name rather than inherited: the guard's standard input is
// the lifeline, and the program's output may not be its standard error.
type terminal struct {
	// modes are the handles whose mode is saved and restored: the one tty on
	// Unix, the console's input and output on Windows.
	modes []*os.File
	out   io.Writer
}

func (t terminal) save() ([]*term.State, error) {
	states := make([]*term.State, len(t.modes))
	for i, f := range t.modes {
		s, err := term.GetState(int(f.Fd()))
		if err != nil {
			return nil, fmt.Errorf("read the terminal's state: %w", err)
		}
		states[i] = s
	}
	return states, nil
}

func (t terminal) restore(states []*term.State) {
	for i, f := range t.modes {
		_ = term.Restore(int(f.Fd()), states[i])
	}
}

func (t terminal) close() {
	for _, f := range t.modes {
		_ = f.Close()
	}
}
