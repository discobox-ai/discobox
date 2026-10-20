package main

import "testing"

// Every recorded case is a job the judge could be asked, checked the way the
// runner checks it before a model reads it: a case that would not load is
// found here rather than by whoever next pays for a run.
func TestTheRecordedCasesLoad(t *testing.T) {
	t.Parallel()
	cases, err := loadCases("../../../test/judge-evals/cases", "*")
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
}
