//go:build !linux

package agent

// CollectAncestry is currently implemented only for Linux. Other platforms can
// still use explicit environment detection.
func CollectAncestry(int) ([]Process, error) {
	return nil, nil
}
