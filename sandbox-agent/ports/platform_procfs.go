//go:build !darwin

package ports

// platformScanner is Linux's: procfs. Windows has no sandbox of its own yet,
// and finding no /proc there reports no ports.
func platformScanner() Scanner { return Procfs{Root: "/proc"} }
