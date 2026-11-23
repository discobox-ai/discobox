package harness

import "testing"

func TestRejectionRuleValidateHost(t *testing.T) {
	for host, ok := range map[string]bool{
		"api.example.com":     true,
		"api.example.com.":    true, // the same host, fully qualified
		"API.example.com":     true,
		"api.example.com:443": false,
		"https://example.com": false,
		"example.com/x":       false,
		".":                   false,
		"":                    false,
	} {
		err := RejectionRule{Host: host}.Validate()
		if (err == nil) != ok {
			t.Errorf("RejectionRule{Host: %q}.Validate() = %v, want ok=%v", host, err, ok)
		}
	}
}
