//go:build windows

package client

import "os"

// Windows has no job-control or user signals, so the fake console recognizes
// no suspend signal and the tests that need one do not build here.
var (
	testSuspendSignal os.Signal
	testUnnamedSignal os.Signal
)
