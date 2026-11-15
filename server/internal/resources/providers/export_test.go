package providers

import "time"

// SetProviderCheckTimeout shortens a check's bound for a test, and restores
// it when the test ends.
func SetProviderCheckTimeout(cleanup func(func()), d time.Duration) {
	was := providerCheckTimeout
	providerCheckTimeout = d
	cleanup(func() { providerCheckTimeout = was })
}
