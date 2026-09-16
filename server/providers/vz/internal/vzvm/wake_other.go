//go:build !darwin || !cgo

package vzvm

type WakeMonitor struct{}

func NewWakeMonitor() (*WakeMonitor, error) { return nil, unsupported() }

func (*WakeMonitor) Events() <-chan struct{} { return nil }

func (*WakeMonitor) Close() {}
