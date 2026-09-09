package endpoint

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// The level is written by an operator in a flag or a config file, so every
// spelling of it that is accepted has to mean what it says and every one that
// is not has to say so.
func TestParseIrohLogLevel(t *testing.T) {
	for value, want := range map[string]IrohLogLevel{
		"":         IrohLogOff,
		"off":      IrohLogOff,
		"  Debug ": IrohLogDebug,
		"WARN":     IrohLogWarn,
		"warning":  IrohLogWarn,
		"trace":    IrohLogTrace,
	} {
		got, err := ParseIrohLogLevel(value)
		if err != nil {
			t.Fatalf("ParseIrohLogLevel(%q) error = %v", value, err)
		}
		if got != want {
			t.Fatalf("ParseIrohLogLevel(%q) = %s, want %s", value, got, want)
		}
	}
	got, err := ParseIrohLogLevel("verbose")
	if err == nil {
		t.Fatalf("ParseIrohLogLevel(\"verbose\") = %s, want an error", got)
	}
	// The error lists what is accepted, because a level nobody can spell is a
	// level nobody turns on.
	if !strings.Contains(err.Error(), "debug") {
		t.Fatalf("error = %v, want it to list the levels", err)
	}
}

// Logging is off unless it was asked for, and a level admits everything at or
// below it and nothing above. The default matters most: this writes to stderr,
// underneath whatever full-screen terminal a CLI is drawing.
func TestIrohLogLevelFilters(t *testing.T) {
	var out bytes.Buffer
	restore := useIrohLog(t, IrohLogInfo, &out)
	defer restore()

	irohLogf(IrohLogWarn, "connect: %s", "refused")
	irohLogf(IrohLogInfo, "bind: bound")
	irohLogf(IrohLogDebug, "stream: opened")

	logged := out.String()
	for _, want := range []string{"connect: refused", "bind: bound"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log = %q, want it to carry %q", logged, want)
		}
	}
	if strings.Contains(logged, "stream: opened") {
		t.Fatalf("log = %q, want nothing above the level that was set", logged)
	}
	if !irohLogEnabled(IrohLogWarn) || irohLogEnabled(IrohLogTrace) {
		t.Fatal("irohLogEnabled disagrees with what irohLogf printed")
	}
}

func TestIrohLogOffPrintsNothing(t *testing.T) {
	var out bytes.Buffer
	restore := useIrohLog(t, IrohLogOff, &out)
	defer restore()

	irohLogf(IrohLogError, "bind: %v", "failed")
	if out.Len() != 0 {
		t.Fatalf("log = %q, want silence when logging is off", out.String())
	}
}

// useIrohLog points the transport's logger at a buffer without touching the
// native library's process-global subscriber, which [SetIrohLogging] installs
// once and for the life of the process — turning it on here would leave every
// other test in this package writing iroh's tracing to stderr.
func useIrohLog(t *testing.T, level IrohLogLevel, out *bytes.Buffer) func() {
	t.Helper()
	irohLogMu.Lock()
	previousLevel, previousWriter := irohLogLevel, irohLogWriter
	irohLogLevel = level
	irohLogWriter = log.New(out, "iroh: ", 0)
	irohLogMu.Unlock()
	return func() {
		irohLogMu.Lock()
		irohLogLevel, irohLogWriter = previousLevel, previousWriter
		irohLogMu.Unlock()
	}
}
