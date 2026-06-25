//go:build windows

package cli

import "os"

func proxiedSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

func signalName(sig os.Signal) (string, bool) {
	if sig == os.Interrupt {
		return "INT", true
	}
	return "", false
}
