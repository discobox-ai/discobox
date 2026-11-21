package harness

import "testing"

func TestRejectionRuleValidateHost(t *testing.T) {
	for _, tc := range []struct {
		host string
		ok   bool
	}{
		{"api.example.com", true},
		{"API.example.com", true},
		{"api.example.com.", true}, // the same host, fully qualified
		{"", false},
		{"api.example.com:443", false},
		{"https://api.example.com", false},
		{"api.example.com/v1", false},
	} {
		err := RejectionRule{Host: tc.host}.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("Validate(host %q) = %v, want ok=%v", tc.host, err, tc.ok)
		}
	}
}
