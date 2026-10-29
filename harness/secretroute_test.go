package harness

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/discobox-ai/discobox/pkg/harnesscapture"
)

// A secret an image declares reads its routes in the shape routes have now,
// and one an older image declares as it was meant: a host and every host
// beneath it, and a route still declared with a use, which was judged, as
// blocked, never as an open route (ADR 26-10-27-880 §8).
func TestADeclaredSecretReadsItsRoutes(t *testing.T) {
	for _, tc := range []struct {
		declared string
		want     []harnesscapture.Route
	}{
		{`[{"hosts":["githubcopilot.com"],"paths":["/mcp/**"],"blocked":true}]`,
			[]harnesscapture.Route{{Hosts: []string{"githubcopilot.com"}, Paths: []string{"/mcp/**"}, Blocked: true}}},
		{`[{"host":"githubcopilot.com","path":"/mcp","use":"read-only queries"}, {"host":"github.com","use":"  "}]`,
			[]harnesscapture.Route{
				{Hosts: []string{"*.githubcopilot.com", "githubcopilot.com"}, Paths: []string{"/mcp/**"}, Blocked: true},
				{Hosts: []string{"*.github.com", "github.com"}},
			}},
	} {
		var got Secret
		if err := json.Unmarshal([]byte(`{"name":"TOKEN","routes":`+tc.declared+`}`), &got); err != nil {
			t.Fatalf("%s: %v", tc.declared, err)
		}
		if !reflect.DeepEqual(got.Routes, tc.want) {
			t.Fatalf("%s = %+v, want %+v", tc.declared, got.Routes, tc.want)
		}
	}
}
