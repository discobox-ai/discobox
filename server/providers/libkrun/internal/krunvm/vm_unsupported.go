//go:build !(linux && amd64)

package krunvm

// Run is unavailable off linux/amd64. The launcher subcommand is compiled into
// every server binary anyway, so that a manifest reaching the wrong platform is
// refused with the provider's own error rather than an unknown-command one.
func Run(Config) error { return ErrUnsupported }
