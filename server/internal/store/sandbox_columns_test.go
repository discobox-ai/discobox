package store

import (
	"slices"
	"testing"
)

// A state report writes back every observed column from the row it loaded, so
// a meta column among them would let a report undo a meta write that committed
// while it ran (ADR 0136 §4). Both lists are omitted by UpdateSandbox.
func TestStateReportsNeverWriteTheMetaColumns(t *testing.T) {
	for _, column := range sandboxMetaColumns {
		if slices.Contains(observedSandboxColumns, column) {
			t.Errorf("%s is written by state reports; it is the meta copy's alone", column)
		}
		if !slices.Contains(sandboxOmittedColumns, column) {
			t.Errorf("%s is not omitted by UpdateSandbox", column)
		}
	}
	for _, column := range observedSandboxColumns {
		if !slices.Contains(sandboxOmittedColumns, column) {
			t.Errorf("%s is not omitted by UpdateSandbox", column)
		}
	}
}
