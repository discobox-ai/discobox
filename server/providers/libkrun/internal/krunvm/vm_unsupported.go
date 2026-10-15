//go:build !(linux && amd64)

package krunvm

// Run is unavailable off linux/amd64. The launcher subcommand is compiled into
// every server binary anyway, so that a manifest reaching the wrong platform is
// refused with the provider's own error rather than an unknown-command one.
func Run(Config) error { return ErrUnsupported }

// checkKVM is never reached off linux/amd64: CheckKVM refuses the platform
// first.
func checkKVM() error { return ErrUnsupported }

// CheckLibrary is unavailable off linux/amd64, for the reason Run is.
func CheckLibrary(string) error { return ErrUnsupported }
