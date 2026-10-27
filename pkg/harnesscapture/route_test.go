package harnesscapture

import (
	"encoding/json"
	"testing"
)

// A save-back route still declared with a use was judged; with nothing to
// judge it, it reads as blocked, never as an open route (ADR 26-10-27-880 §8).
func TestADeclaredRouteWithAUseReadsAsBlocked(t *testing.T) {
	for _, tc := range []struct {
		declared string
		want     Route
	}{
		{`{"host":"githubcopilot.com"}`, Route{Host: "githubcopilot.com"}},
		{`{"host":"githubcopilot.com","path":"/mcp","blocked":true}`, Route{Host: "githubcopilot.com", Path: "/mcp", Blocked: true}},
		{`{"host":"githubcopilot.com","path":"/mcp","use":"read-only queries"}`, Route{Host: "githubcopilot.com", Path: "/mcp", Blocked: true}},
	} {
		var got Route
		if err := json.Unmarshal([]byte(tc.declared), &got); err != nil {
			t.Fatalf("%s: %v", tc.declared, err)
		}
		if got != tc.want {
			t.Fatalf("%s = %+v, want %+v", tc.declared, got, tc.want)
		}
	}
}
