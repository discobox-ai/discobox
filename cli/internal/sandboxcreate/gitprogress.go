package sandboxcreate

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/sandboxgit"
)

// A fetch from a remote, and the push of what it fetched, are the two
// transfers of a delivery that can take minutes: the first create from a
// remote neither this client nor its pool has seen moves the whole history,
// twice. Git says how far each has got; this passes it on, so the line a
// frontend shows is moving rather than a single word for a minute.

// gitMeter is one of git's progress meters:
//
//	Receiving objects:  42% (1234/2938), 12.30 MiB | 1.20 MiB/s
//	remote: Compressing objects: 100% (87/87), done.
var gitMeter = regexp.MustCompile(`^(?:remote: )?([A-Z][a-z]+ [a-z]+):\s+(\d+)% \(\d+/\d+\)(?:, ([0-9.]+ [KMGT]?i?B)(?: \| ([0-9.]+ [KMGT]?i?B/s))?)?`)

// gitProgressInterval is the least time between two reports of one transfer.
// A meter redraws many times a second, and a status line that changes that
// fast cannot be read.
const gitProgressInterval = 200 * time.Millisecond

// progressStep is step with how far its transfer has got, in a status line's
// words: "fetching source: receiving objects 42%, 12.30 MiB at 1.20 MiB/s".
func progressStep(step Step, meter string) (Step, bool) {
	match := gitMeter.FindStringSubmatch(strings.TrimSpace(meter))
	if match == nil {
		return "", false
	}
	text := string(step) + ": " + strings.ToLower(match[1]) + " " + match[2] + "%"
	if match[3] != "" {
		text += ", " + match[3]
	}
	if match[4] != "" {
		text += " at " + match[4]
	}
	return Step(text), true
}

// meterWriter is git's stderr during a transfer: it reports each meter as the
// step it belongs to, and keeps the tail of everything else for the error.
type meterWriter struct {
	step   Step
	report Report

	mu       sync.Mutex
	line     []byte
	tail     []byte
	reported time.Time
	last     Step
}

func (w *meterWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b != '\r' && b != '\n' {
			w.line = append(w.line, b)
			continue
		}
		w.flush()
	}
	return len(p), nil
}

// flush handles one finished line. The caller holds w.mu.
func (w *meterWriter) flush() {
	line := string(w.line)
	w.line = w.line[:0]
	if strings.TrimSpace(line) == "" {
		return
	}
	step, ok := progressStep(w.step, line)
	if !ok {
		// Not a meter: what git said, which is the error if this fails.
		w.tail = append(append(w.tail, line...), '\n')
		if over := len(w.tail) - 4096; over > 0 {
			w.tail = w.tail[over:]
		}
		return
	}
	if step == w.last || time.Since(w.reported) < gitProgressInterval {
		return
	}
	w.last, w.reported = step, time.Now()
	w.report.step(step)
}

func (w *meterWriter) message() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flush()
	return strings.TrimSpace(string(w.tail))
}

// gitTransfer runs a git fetch or push in dir with its progress reported as
// step. args must ask git for progress (--progress): its stderr is not a
// terminal here, and it reports none to anything else unasked.
func gitTransfer(ctx context.Context, dir string, env map[string]string, step Step, report Report, args ...string) error {
	//nolint:gosec // The executable is fixed; the arguments are this package's own refspecs, URLs and switches.
	cmd := sandboxgit.Command(ctx, dir, env, args...)
	// The meters are git's English: a translated one would not parse, and
	// would simply not be reported.
	cmd.Env = append(cmd.Env, "LC_ALL=C")
	stderr := &meterWriter{step: step, report: report}
	var stdout bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, stderr
	if err := cmd.Run(); err != nil {
		if message := stderr.message(); message != "" {
			return fmt.Errorf("%w: %s", err, message)
		}
		return err
	}
	return nil
}
