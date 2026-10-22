package endpoint

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"

	iroh "github.com/discobox-ai/iroh-go"
)

// IrohLogEnv names the environment variable that turns transport logging on:
// off, error, warn, info, debug, or trace.
//
// It exists because an iroh connection fails in layers, and by the time a
// caller sees an error every layer below it has already decided something in
// silence — which socket was bound, whether a relay was reached, which
// addresses were dialed, whether the peer answered or refused. Nothing above
// the transport can reconstruct that from the one error it is handed.
//
// The variable is read by whoever configures the endpoint, not here: the CLI's
// `--iroh-log` and the server's `iroh.logLevel` both resolve to
// [SetIrohLogging], so there is one place that decides and one place that
// prints.
const IrohLogEnv = "DISCOBOX_IROH_LOG"

// IrohLogLevel is how much the transport says about itself. It doubles as the
// level of iroh's own tracing, which is the layer below this one: an operator
// asking for debug wants both halves, and two knobs for one question is one
// knob too many.
type IrohLogLevel int

const (
	// IrohLogOff is the default. A transport that logs by default would write
	// to a CLI's stderr underneath a full-screen terminal.
	IrohLogOff IrohLogLevel = iota
	IrohLogError
	IrohLogWarn
	IrohLogInfo
	IrohLogDebug
	IrohLogTrace
)

// irohLogLevelNames is the spelling of each level, indexed by the level. It is
// both what [ParseIrohLogLevel] accepts and what an error lists, so the two
// cannot drift.
var irohLogLevelNames = [...]string{"off", "error", "warn", "info", "debug", "trace"}

func (l IrohLogLevel) String() string {
	if l < 0 || int(l) >= len(irohLogLevelNames) {
		return fmt.Sprintf("IrohLogLevel(%d)", int(l))
	}
	return irohLogLevelNames[l]
}

// ParseIrohLogLevel reads a level by name. The empty string is [IrohLogOff],
// so an unset variable and an explicit "off" mean the same thing.
func ParseIrohLogLevel(value string) (IrohLogLevel, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "":
		return IrohLogOff, nil
	case "warning":
		// tracing's own spelling, and the one half of these operators type.
		return IrohLogWarn, nil
	}
	for level, name := range irohLogLevelNames {
		if normalized == name {
			return IrohLogLevel(level), nil
		}
	}
	return IrohLogOff, fmt.Errorf("unknown log level %q; expected one of %s", value, strings.Join(irohLogLevelNames[:], ", "))
}

var (
	irohLogMu     sync.RWMutex
	irohLogLevel  = IrohLogOff
	irohLogWriter *log.Logger
)

// SetIrohLogging turns transport logging on at level, writing to out (stderr
// when nil), and turns iroh's own tracing on at the same level.
//
// The native library installs a process-global tracing subscriber the first
// time it is asked, so only the first call changes *its* verbosity; this
// package's own level can be changed at any time. That asymmetry is the
// library's and is why the two are set together here rather than separately by
// each caller.
func SetIrohLogging(level IrohLogLevel, out io.Writer) error {
	if out == nil {
		out = os.Stderr
	}
	irohLogMu.Lock()
	irohLogLevel = level
	// No date: a diagnostic read beside a server log wants to line up with the
	// lines around it, and microseconds are what separate two events in one
	// handshake.
	irohLogWriter = log.New(out, "iroh: ", log.Ltime|log.Lmicroseconds)
	irohLogMu.Unlock()
	if level == IrohLogOff {
		return nil
	}
	return iroh.SetLogLevel(nativeIrohLogLevel(level))
}

// nativeIrohLogLevel maps our level onto the library's. The two happen to
// agree numerically; spelling the mapping out is what keeps a reordering of
// either list from silently changing what an operator asked for.
func nativeIrohLogLevel(level IrohLogLevel) iroh.LogLevel {
	switch level {
	case IrohLogError:
		return iroh.LogError
	case IrohLogWarn:
		return iroh.LogWarn
	case IrohLogInfo:
		return iroh.LogInfo
	case IrohLogDebug:
		return iroh.LogDebug
	case IrohLogTrace:
		return iroh.LogTrace
	default:
		return iroh.LogOff
	}
}

// irohLogEnabled reports whether a message at this level would be printed, for
// a caller whose message costs something to build.
func irohLogEnabled(level IrohLogLevel) bool {
	irohLogMu.RLock()
	defer irohLogMu.RUnlock()
	return irohLogLevel != IrohLogOff && level <= irohLogLevel
}

// irohLogf writes one transport event. The layer is the first word of every
// message — bind, relay, connect, stream, accept — so a log can be read as the
// sequence of layers a connection got through.
func irohLogf(level IrohLogLevel, format string, args ...any) {
	irohLogMu.RLock()
	enabled := irohLogLevel != IrohLogOff && level <= irohLogLevel
	writer := irohLogWriter
	irohLogMu.RUnlock()
	if !enabled || writer == nil {
		return
	}
	writer.Printf("%-5s %s", level, fmt.Sprintf(format, args...))
}
