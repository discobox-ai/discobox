//go:build windows

package client

import (
	"os"
	"os/signal"
)

// proxiedSignals is only os.Interrupt on Windows, which has no others to
// forward. Ctrl-C and Ctrl-Z still reach the remote as bytes whenever the
// console is in raw mode, which is the common case.
func proxiedSignals() []os.Signal { return []os.Signal{os.Interrupt} }

func (c *OSConsole) NotifySignals(ch chan<- os.Signal) { signal.Notify(ch, proxiedSignals()...) }

func (c *OSConsole) StopSignals(ch chan<- os.Signal) { signal.Stop(ch) }

// IsSuspendSignal is always false: Windows has no job control, so nothing
// suspends and Suspend is never reached.
func (c *OSConsole) IsSuspendSignal(os.Signal) bool { return false }

func (c *OSConsole) SignalName(sig os.Signal) (string, bool) {
	if sig == os.Interrupt {
		return "INT", true
	}
	return "", false
}

func (c *OSConsole) Suspend() {}
