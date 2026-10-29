package cli

import "testing"

// --raw is parsed as it is set: env or file, and none only where a binding
// is changed back to a sentinel.
func TestRawFlagTakesADelivery(t *testing.T) {
	for _, tc := range []struct {
		in       string
		canClear bool
		ok       bool
	}{
		{"env", false, true}, {"file", false, true}, {"none", false, false}, {"raw", false, false},
		{"none", true, true}, {"env", true, true}, {"", true, false},
	} {
		f := rawFlag{canClear: tc.canClear}
		if err := f.Set(tc.in); (err == nil) != tc.ok {
			t.Errorf("Set(%q) canClear=%v = %v, want ok=%v", tc.in, tc.canClear, err, tc.ok)
		}
		if tc.ok && (!f.set || f.value != tc.in) {
			t.Errorf("Set(%q) left %+v", tc.in, f)
		}
	}
}
