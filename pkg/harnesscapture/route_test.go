package harnesscapture

import (
	"encoding/json"
	"reflect"
	"testing"
)

// A declared route reads in the shape routes have now. One a bundle built
// before host patterns declares reads as it was meant, the host and every
// host beneath it and the path and below; one still declared with a use was
// judged, and with nothing to judge it reads as blocked, never as an open
// route (ADR 26-10-27-880 §8).
func TestADeclaredRouteReadsInTheShapeRoutesHaveNow(t *testing.T) {
	for _, tc := range []struct {
		declared string
		want     Route
	}{
		{`{"hosts":["githubcopilot.com"],"methods":["POST"],"paths":["/v1/**"]}`, Route{Hosts: []string{"githubcopilot.com"}, Methods: []string{"POST"}, Paths: []string{"/v1/**"}}},
		{`{"host":"githubcopilot.com"}`, Route{Hosts: []string{"*.githubcopilot.com", "githubcopilot.com"}}},
		{`{"host":"githubcopilot.com","path":"/mcp","blocked":true}`, Route{Hosts: []string{"*.githubcopilot.com", "githubcopilot.com"}, Paths: []string{"/mcp/**"}, Blocked: true}},
		{`{"host":"githubcopilot.com","path":"/mcp","use":"read-only queries"}`, Route{Hosts: []string{"*.githubcopilot.com", "githubcopilot.com"}, Paths: []string{"/mcp/**"}, Blocked: true}},
		{`{"host":"*"}`, Route{Hosts: []string{"*"}}},
	} {
		var got Route
		if err := json.Unmarshal([]byte(tc.declared), &got); err != nil {
			t.Fatalf("%s: %v", tc.declared, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s = %+v, want %+v", tc.declared, got, tc.want)
		}
	}
	// A legacy path holding * was literal, and would now be a glob.
	var refused Route
	if err := json.Unmarshal([]byte(`{"host":"github.com","path":"/a*b"}`), &refused); err == nil {
		t.Fatalf("a legacy route FromLegacy cannot say read as %+v", refused)
	}
}
