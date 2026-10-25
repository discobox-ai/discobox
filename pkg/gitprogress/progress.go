package gitprogress

import (
	"bytes"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Stage is where a clone is, as git's progress output tells it.
type Stage string

const (
	// Counting is the remote enumerating, counting and compressing what
	// it will send: nothing has arrived yet, and on a large repository this
	// is seconds of the remote's own time.
	Counting Stage = "counting"
	// Receiving is objects arriving.
	Receiving Stage = "receiving"
	// Resolving is every object arrived and git resolving deltas.
	Resolving Stage = "resolving"
)

// Progress is how far a source's clone has got. Objects and
// ObjectsTotal count within the stage — deltas, when resolving — and git
// knows the total before the stage starts, so the pair is progress toward a
// fixed target. Bytes is what has been received, and stays at its last value
// once receiving ends; it is zero until git starts showing a throughput, which
// it does only once a clone is slow enough to need one.
type Progress struct {
	Stage        Stage
	Objects      int64
	ObjectsTotal int64
	Bytes        int64
}

// progressLine matches one of git's progress meters, from the remote or its
// own: "Receiving objects:  50% (17739/35477), 14.90 MiB | 5.00 MiB/s", or a
// count with no total yet, "remote: Enumerating objects: 35477".
var progressLine = regexp.MustCompile(`^(?:remote: )?(Enumerating|Counting|Compressing|Receiving|Resolving) (?:objects|deltas):\s+(?:\d+% \((\d+)/(\d+)\)|(\d+))(?:, ([\d.]+) (bytes|KiB|MiB|GiB|TiB))?`)

// progressStages maps git's meter names to the stages a status line tells
// apart: everything the remote does before it sends is one wait.
var progressStages = map[string]Stage{
	"Enumerating": Counting,
	"Counting":    Counting,
	"Compressing": Counting,
	"Receiving":   Receiving,
	"Resolving":   Resolving,
}

// maxCloneMessages bounds what a clone's stderr keeps for its error. Progress
// meters are not kept at all — they are reported instead — so this is only
// git's own messages, which a failure is explained by. The end is what is
// kept: git's fatal line is the last thing it writes.
const maxCloneMessages = 8 << 10

// Writer is a clone's stderr. git redraws each meter in place with a
// carriage return, so every line, ended by either, is parsed: a meter updates
// the progress and is reported, and anything else is a message kept for the
// error a failed clone returns.
type Writer struct {
	Report   func(Progress)
	progress Progress
	partial  []byte
	messages bytes.Buffer
	last     string
}

func (w *Writer) Write(p []byte) (int, error) {
	size := len(p)
	for len(p) > 0 {
		end := bytes.IndexAny(p, "\r\n")
		if end < 0 {
			w.appendPartial(p)
			break
		}
		w.appendPartial(p[:end])
		w.line(string(w.partial))
		w.partial = w.partial[:0]
		p = p[end+1:]
	}
	return size, nil
}

// appendPartial bounds even an unfinished line from an untrusted Git remote.
func (w *Writer) appendPartial(p []byte) {
	if len(p) > maxCloneMessages {
		p = p[len(p)-maxCloneMessages:]
		w.partial = w.partial[:0]
	}
	if excess := len(w.partial) + len(p) - maxCloneMessages; excess > 0 {
		w.partial = w.partial[excess:]
	}
	w.partial = append(w.partial, p...)
}

// String is git's messages, the meters left out: what rawOutput would have
// returned as stderr.
func (w *Writer) String() string {
	w.line(string(w.partial))
	w.partial = nil
	return w.messages.String()
}

func (w *Writer) line(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	match := progressLine.FindStringSubmatch(line)
	if match == nil {
		w.message(line)
		return
	}
	next := w.progress
	next.Stage = progressStages[match[1]]
	if next.Stage != w.progress.Stage {
		next.Objects, next.ObjectsTotal = 0, 0
	}
	if match[2] != "" {
		next.Objects, _ = strconv.ParseInt(match[2], 10, 64)
		next.ObjectsTotal, _ = strconv.ParseInt(match[3], 10, 64)
	} else {
		next.Objects, _ = strconv.ParseInt(match[4], 10, 64)
		next.ObjectsTotal = 0
	}
	if match[5] != "" {
		next.Bytes = progressBytes(match[5], match[6])
	}
	if next == w.progress {
		return
	}
	w.progress = next
	if w.Report != nil {
		w.Report(next)
	}
}

// message keeps one of git's lines for the error. A meter this parser does
// not know is redrawn in place like any other, so a line repeating the one
// before it is kept once; and past the bound the oldest lines go, not the
// newest.
func (w *Writer) message(line string) {
	if line == w.last {
		return
	}
	w.last = line
	w.messages.WriteString(line)
	w.messages.WriteByte('\n')
	if excess := w.messages.Len() - maxCloneMessages; excess > 0 {
		kept := w.messages.Bytes()[excess:]
		if cut := bytes.IndexByte(kept, '\n'); cut >= 0 && cut+1 < len(kept) {
			kept = kept[cut+1:]
		}
		w.messages = *bytes.NewBuffer(slices.Clone(kept))
	}
}

// progressBytes reads git's rendering of a byte count back. It is rounded to
// two places, which is as much as a status line shows anyway.
func progressBytes(value, unit string) int64 {
	number, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	scale := map[string]float64{"bytes": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}[unit]
	return int64(number * scale)
}
