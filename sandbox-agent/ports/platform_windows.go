package ports

import "time"

// platformScanner is Windows's. Windows has no sandbox of its own yet: this is
// procfs, which finds no /proc there and reports no ports.
func platformScanner() Scanner { return Procfs{Root: "/proc"} }

// platformInterval is Linux's, for want of a reason to differ.
const platformInterval = 500 * time.Millisecond
