package ports

import "time"

// platformScanner is Linux's: sock_diag for listening TCP sockets, procfs for
// UDP ones and for a kernel without sock_diag (see sockDiag).
func platformScanner() Scanner {
	return &sockDiag{Procfs: Procfs{Root: "/proc"}, dump: sockDiagDump}
}

// platformInterval is half a second: a sock_diag dump of the listening
// sockets and two small procfs reads, so a sign-in's callback port is seen
// before the browser is sent to it (ADR 26-11-03-815 §1).
const platformInterval = 500 * time.Millisecond
