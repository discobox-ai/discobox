//go:build !darwin

package ports

import "time"

// platformScanner is Linux's: procfs. Windows has no sandbox of its own yet,
// and finding no /proc there reports no ports.
func platformScanner() Scanner { return Procfs{Root: "/proc"} }

// platformInterval is half a second: a procfs read and a uid comparison, which
// configure boxes already ran at this rate (ADR 26-11-03-815 §1).
const platformInterval = 500 * time.Millisecond
