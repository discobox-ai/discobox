package gateway

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func anthropic() Route {
	return Normalize(Route{
		Name:   "anthropic",
		Match:  "https://API.anthropic.com:443/v1/",
		Target: "https://gateway.example.com/anthropic/v1",
		RequestHeaders: HeaderModifier{
			Remove: []string{"x-api-key"},
			Set: []Header{
				{Name: "cf-aig-authorization", Env: "DISCOBOX_GATEWAY_CF_TOKEN"},
				{Name: "x-team", Value: "platform"},
			},
		},
	})
}

func TestNormalizeWritesTheCanonicalForm(t *testing.T) {
	route := anthropic()
	if route.Match != "https://api.anthropic.com/v1" {
		t.Fatalf("match = %q", route.Match)
	}
	if route.RequestHeaders.Remove[0] != "X-Api-Key" || route.RequestHeaders.Set[0].Name != "Cf-Aig-Authorization" {
		t.Fatalf("headers = %+v", route.RequestHeaders)
	}
	if err := route.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMatchIsOnSegmentBoundariesAndKeepsTheRest(t *testing.T) {
	table := NewTable([]Route{anthropic()})
	cases := []struct {
		in, out string
	}{
		{"https://api.anthropic.com/v1/messages?beta=true", "https://gateway.example.com/anthropic/v1/messages?beta=true"},
		{"https://api.anthropic.com:443/v1/messages/count_tokens", "https://gateway.example.com/anthropic/v1/messages/count_tokens"},
		{"https://api.anthropic.com/v1", "https://gateway.example.com/anthropic/v1"},
		{"https://api.anthropic.com/v1beta/messages", ""},
		// The host as policy reads it, and the path as a server reads it: a
		// sandbox cannot spell its way around the gateway, nor out of the
		// target's prefix.
		{"https://api.anthropic.com./v1/messages", "https://gateway.example.com/anthropic/v1/messages"},
		{"https://api.anthropic.com//v1//messages", "https://gateway.example.com/anthropic/v1/messages"},
		{"https://api.anthropic.com/./v1/messages/", "https://gateway.example.com/anthropic/v1/messages/"},
		{"https://api.anthropic.com/x/../v1/messages", "https://gateway.example.com/anthropic/v1/messages"},
		{"https://api.anthropic.com/v1/%2e%2e/v1/messages", "https://gateway.example.com/anthropic/v1/messages"},
		{"https://api.anthropic.com/v1/../../admin", ""},
		{"https://api.anthropic.com/%761/messages", "https://gateway.example.com/anthropic/v1/messages"},
		{"https://api.anthropic.com/v%31/m%65ssages", "https://gateway.example.com/anthropic/v1/messages"},
		{"https://api.anthropic.com/v1/caf%C3%A9", "https://gateway.example.com/anthropic/v1/caf%C3%A9"},
		{"https://api.anthropic.com/v1/messages/../../../admin", ""},
		{"https://api.anthropic.com/v2/messages", ""},
		{"http://api.anthropic.com/v1/messages", ""},
		{"https://api.anthropic.com:8443/v1/messages", ""},
		{"https://other.example.com/v1/messages", ""},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		route, forwarded, ok, err := table.Match(u)
		if err != nil {
			t.Errorf("%s: %v", tc.in, err)
			continue
		}
		if tc.out == "" {
			if ok {
				t.Errorf("%s matched %s", tc.in, route.Name)
			}
			continue
		}
		if !ok {
			t.Errorf("%s matched nothing", tc.in)
			continue
		}
		if got := forwarded.String(); got != tc.out {
			t.Errorf("%s went to %s, want %s", tc.in, got, tc.out)
		}
	}
}

func TestTheLongestPrefixWins(t *testing.T) {
	table := NewTable([]Route{
		Normalize(Route{Name: "all", Match: "https://api.openai.com", Target: "https://gw.example.com/openai"}),
		Normalize(Route{Name: "chat", Match: "https://api.openai.com/v1/chat", Target: "https://chat.example.com"}),
	})
	u, _ := url.Parse("https://api.openai.com/v1/chat/completions")
	route, forwarded, ok, _ := table.Match(u)
	if !ok || route.Name != "chat" || forwarded.String() != "https://chat.example.com/completions" {
		t.Fatalf("got %v %v %v", route.Name, forwarded, ok)
	}
	u, _ = url.Parse("https://api.openai.com/v1/models")
	route, forwarded, ok, _ = table.Match(u)
	if !ok || route.Name != "all" || forwarded.String() != "https://gw.example.com/openai/v1/models" {
		t.Fatalf("got %v %v %v", route.Name, forwarded, ok)
	}
	u, _ = url.Parse("https://api.openai.com/v1/chat")
	if _, forwarded, _, _ = table.Match(u); forwarded.String() != "https://chat.example.com/" {
		t.Fatalf("an exact match onto a root target went to %v", forwarded)
	}
}

// A match or target is stored in the spelling requests are matched in.
func TestNormalizeCleansPaths(t *testing.T) {
	route := Normalize(Route{Name: "g", Match: "https://h.example.com.//%761/./x/../", Target: "https://gw.example.com/a/../b"})
	if route.Match != "https://h.example.com/v1" || route.Target != "https://gw.example.com/b" {
		t.Fatalf("normalized %s -> %s", route.Match, route.Target)
	}
	table := NewTable([]Route{route})
	u, _ := url.Parse("https://h.example.com/v1/messages")
	if _, forwarded, ok, _ := table.Match(u); !ok || forwarded.String() != "https://gw.example.com/b/messages" {
		t.Fatalf("matched %v %v", forwarded, ok)
	}
}

func TestValidateRefuses(t *testing.T) {
	base := func(mut func(*Route)) Route {
		route := anthropic()
		mut(&route)
		return Normalize(route)
	}
	cases := map[string]Route{
		"bad name":            base(func(r *Route) { r.Name = "Anthropic" }),
		"ftp match":           base(func(r *Route) { r.Match = "ftp://api.anthropic.com" }),
		"no host":             base(func(r *Route) { r.Target = "https:///v1" }),
		"query":               base(func(r *Route) { r.Target = "https://gw.example.com/?a=b" }),
		"encoded slash":       base(func(r *Route) { r.Match = "https://api.anthropic.com/v1%2Fx" }),
		"user":                base(func(r *Route) { r.Target = "https://me:pw@gw.example.com" }),
		"host header":         base(func(r *Route) { r.RequestHeaders.Set = []Header{{Name: "Host", Value: "x"}} }),
		"hop-by-hop":          base(func(r *Route) { r.RequestHeaders.Remove = []string{"Connection"} }),
		"both value and env":  base(func(r *Route) { r.RequestHeaders.Add = []Header{{Name: "X", Value: "v", Env: "DISCOBOX_GATEWAY_X"}} }),
		"neither":             base(func(r *Route) { r.RequestHeaders.Add = []Header{{Name: "X"}} }),
		"unprefixed env":      base(func(r *Route) { r.RequestHeaders.Set = []Header{{Name: "X", Env: "DATABASE_URL"}} }),
		"bare prefix":         base(func(r *Route) { r.RequestHeaders.Set = []Header{{Name: "X", Env: EnvPrefix}} }),
		"value with newline":  base(func(r *Route) { r.RequestHeaders.Set = []Header{{Name: "X", Value: "a\nb"}} }),
		"header name a space": base(func(r *Route) { r.RequestHeaders.Remove = []string{"x y"} }),
	}
	for name, route := range cases {
		if err := route.Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, route)
		}
	}
}

func TestEnvAndHeaderNames(t *testing.T) {
	route := anthropic()
	route.RequestHeaders.Add = []Header{{Name: "X-Extra", Env: "DISCOBOX_GATEWAY_CF_TOKEN"}}
	if got := route.Env(); len(got) != 1 || got[0] != "DISCOBOX_GATEWAY_CF_TOKEN" {
		t.Fatalf("env = %v", got)
	}
	if got := strings.Join(route.HeaderNames(), ","); got != "X-Api-Key,Cf-Aig-Authorization,X-Team,X-Extra" {
		t.Fatalf("names = %s", got)
	}
}

// A path a server could read as another, under a gateway's origin, is refused
// rather than matched one way and read another.
func TestMatchRefusesAnAmbiguousPath(t *testing.T) {
	table := NewTable([]Route{anthropic()})
	for _, raw := range []string{
		"https://api.anthropic.com/v1/..%2F..%2Fadmin",
		"https://api.anthropic.com/v1/a%5Cb",
		"https://api.anthropic.com/other%2fpath",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		route, _, ok, err := table.Match(u)
		if !errors.Is(err, ErrAmbiguousPath) || ok || route.Name != "anthropic" {
			t.Errorf("%s: %v %v %v", raw, route.Name, ok, err)
		}
	}
	u, _ := url.Parse("https://other.example.com/a%2Fb")
	if _, _, ok, err := table.Match(u); ok || err != nil {
		t.Fatalf("another origin: %v %v", ok, err)
	}
}

// A route the proxy cannot apply is skipped, and the others still apply.
func TestNewTableSkipsWhatItCannotApply(t *testing.T) {
	bad := anthropic()
	bad.Name = "Bad Name"
	other := Normalize(Route{Name: "openai", Match: "https://api.openai.com/v1", Target: "https://gw.example.com/openai/v1"})
	table := NewTable([]Route{bad, other})
	u, _ := url.Parse("https://api.anthropic.com/v1/messages")
	if _, _, ok, _ := table.Match(u); ok {
		t.Fatal("an invalid route applied")
	}
	u, _ = url.Parse("https://api.openai.com/v1/chat")
	if _, _, ok, _ := table.Match(u); !ok {
		t.Fatal("a valid route beside an invalid one did not apply")
	}
}
