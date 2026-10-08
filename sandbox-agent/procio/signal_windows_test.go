package procio

import (
	"os"
	"strings"
	"testing"
)

// The handle a suspend goes through is the one taken at Start, never a PID
// opened again: once the process is closed a late signal has nothing to reach,
// rather than reaching whatever process Windows gave the PID to next.
func TestSignalAfterCloseReachesNoProcess(t *testing.T) {
	p, err := Start(Options{Command: []string{"cmd", "/c", "exit 0"}, Env: os.Environ()})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	p.Wait()
	p.Close()
	delivery, err := p.Signal("TSTP")
	if delivery.Delivered != "NtSuspendProcess" || err == nil || !strings.Contains(err.Error(), "released") {
		t.Fatalf("signal after close = %+v, %v; want NtSuspendProcess refused as released", delivery, err)
	}
}
