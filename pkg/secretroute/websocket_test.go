package secretroute

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const discordRoute = `GET *.discord.gg,gateway.discord.gg ws=[{"at":"/d/token","when":{"op":2}},{"at":"/d/token","when":{"op":6}}]`

func mustParse(t *testing.T, text string) Route {
	t.Helper()
	route, err := Parse(text)
	if err != nil {
		t.Fatalf("Parse(%q): %v", text, err)
	}
	return route
}

func mustMessage(t *testing.T, text string) *Message {
	t.Helper()
	m, err := ParseMessage([]byte(text))
	if err != nil {
		t.Fatalf("ParseMessage(%q): %v", text, err)
	}
	return m
}

func TestWebSocketRouteTypedRoundTrip(t *testing.T) {
	route := mustParse(t, discordRoute)
	if route.WebSocket == nil || len(route.WebSocket.Places) != 2 {
		t.Fatalf("places = %+v", route.WebSocket)
	}
	if got := Format(route); got != discordRoute {
		t.Fatalf("Format = %s\nwant     %s", got, discordRoute)
	}
	// Whitespace inside the array is part of the JSON value, conditions are
	// sorted, numbers take one spelling, places are deduplicated and sorted,
	// and the blocked suffix follows the array.
	loose := `GET gateway.discord.gg  ws=[ {"when": {"op": 6.0, "s": "x y"}, "at": "/d/token"} ,
		{"at":"/d/token","when":{"op":2}}, {"at":"/d/token","when":{"op":2.00}} ]=blocked`
	got := mustParse(t, loose)
	want := `GET gateway.discord.gg ws=[{"at":"/d/token","when":{"op":2}},{"at":"/d/token","when":{"op":6,"s":"x y"}}]=blocked`
	if Format(got) != want || !got.Blocked {
		t.Fatalf("Format = %s\nwant     %s", Format(got), want)
	}
	if again := mustParse(t, Format(got)); Format(again) != want {
		t.Fatalf("round trip = %s", Format(again))
	}
	// A word may follow the array, and other words may precede it.
	if got := mustParse(t, `api.example.com/socket ws=[{"at":"/auth"}]`); got.WebSocket == nil || got.Paths[0] != "/socket" {
		t.Fatalf("route = %+v", got)
	}
	// The JSON shape survives JSON, as the API and policy files carry it.
	data, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Route
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	normalized, err := Normalize(decoded)
	if err != nil || Format(normalized) != discordRoute {
		t.Fatalf("JSON round trip = %s, %v", Format(normalized), err)
	}
}

func TestWebSocketRouteRefusals(t *testing.T) {
	for _, text := range []string{
		`h ws=[]`,
		`h ws=`,
		`h ws={"at":"/a"}`,
		`h ws=[{"at":"a"}]`,
		`h ws=[{"at":""}]`,
		`h ws=[{"at":"/a/-"}]`,
		`h ws=[{"at":"/a/*"}]`,
		`h ws=[{"at":"/a~2"}]`,
		`h ws=[{"at":"/a~"}]`,
		`h ws=[{"at":"/a","when":{"op":[2]}}]`,
		`h ws=[{"at":"/a","when":{"op":{"x":1}}}]`,
		`h ws=[{"at":"/a","when":{"op":2,"op":3}}]`,
		`h ws=[{"at":"/a","where":{}}]`,
		`h ws=[{"at":"/a","when":{"op":"\ud800"}}]`,
		`h ws=[{"at":"/a"}]x`,
		`h ws=[{"at":"/a"}] ws=[{"at":"/b"}]`,
		`h body=/a ws=[{"at":"/a"}]`,
		`h mcp=* ws=[{"at":"/a"}]`,
		`h ws=[{"at":"/a\u0007"}]`,
	} {
		if route, err := Parse(text); err == nil {
			t.Errorf("Parse(%q) = %s, want an error", text, Format(route))
		}
	}
	if _, err := Normalize(Route{Hosts: []string{"h"}, WebSocket: &WebSocket{}}); err == nil {
		t.Error("an empty WebSocket block normalized")
	}
	if _, err := Normalize(Route{Hosts: []string{"h"}, WebSocket: &WebSocket{Places: []MessagePlace{{At: "/a", When: map[string]json.RawMessage{"op": json.RawMessage(`[1]`)}}}}}); err == nil {
		t.Error("an array condition normalized")
	}
}

func TestParseMessageStrict(t *testing.T) {
	for _, text := range []string{
		`[]`,
		`"x"`,
		`{"a":1} {"b":2}`,
		`{"a":1,"a":2}`,
		`{"a":{"b":1,"b":1}}`,
		`{"a":1,"a":2}`,
		`{"a":"\ud800"}`,
		`{"a":"\udc00\ud800"}`,
		`{"a":01}`,
		`{"a":1.}`,
		`{"a":"` + "\x01" + `"}`,
		`{"a":1e99999999}`,
		"{\"a\":\"\xff\"}",
		strings.Repeat(`{"a":`, 65) + `1` + strings.Repeat(`}`, 65),
	} {
		if _, err := ParseMessage([]byte(text)); !errors.Is(err, ErrNotMessage) {
			t.Errorf("ParseMessage(%q) = %v, want ErrNotMessage", text, err)
		}
	}
	m := mustMessage(t, " \n{\"op\":2.0,\"d\":{\"to\\/ken\":\"t\",\"a~b\":[\"x\",\"\\ud83d\\ude00\"]}}\t")
	got := map[string]string{}
	for _, s := range m.Strings() {
		if !s.Name {
			got[s.At()] = s.Value
		}
	}
	if got["/d/to~1ken"] != "t" || got["/d/a~0b/0"] != "x" || got["/d/a~0b/1"] != "😀" {
		t.Fatalf("strings = %v", got)
	}
	if !m.holds("op", "2") {
		t.Fatalf("op 2.0 does not hold op 2: %v", m.top)
	}
	if _, err := ParseMessage([]byte(strings.Repeat(`{"a":`, 64) + `1` + strings.Repeat(`}`, 64))); err != nil {
		t.Fatalf("64 deep: %v", err)
	}
}

func TestCanonicalNumbers(t *testing.T) {
	same := [][]string{
		{"2", "2.0", "2.000", "20e-1", "0.2e1", "2E0", "2e+0"},
		{"0", "-0", "0.0", "0e10"},
		{"1e25", "10000000000000000000000000", "1.0e25"},
		{"0.001", "1e-3", "10e-4"},
		{"-12.5", "-125e-1"},
		{"1e-7", "0.0000001"},
	}
	for _, group := range same {
		want, err := canonicalScalar([]byte(group[0]))
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range group[1:] {
			if got, err := canonicalScalar([]byte(n)); err != nil || got != want {
				t.Errorf("%s = %q (%v), want %q as %s is", n, got, err, want, group[0])
			}
		}
	}
	// Floats would round these together; exact decimals do not.
	a, _ := canonicalScalar([]byte("9007199254740993"))
	b, _ := canonicalScalar([]byte("9007199254740992"))
	if a == b {
		t.Fatal("distinct large integers compare equal")
	}
	if s, _ := canonicalScalar([]byte(`"ab"`)); s != `"ab"` {
		t.Fatalf("string = %s", s)
	}
}

func discordLists(t *testing.T) [][]Route {
	return [][]Route{{mustParse(t, discordRoute)}}
}

func messageRequest(m *Message, at ...string) Request {
	return Request{Method: "GET", Host: "gateway.discord.gg", URL: "https://gateway.discord.gg/?v=10&encoding=json", Message: m, At: at}
}

func TestDecideMessagePlaces(t *testing.T) {
	identify := mustMessage(t, `{"op":2,"d":{"token":"S","intents":1}}`)
	resume := mustMessage(t, `{"op":6.0,"d":{"token":"S","seq":4}}`)
	presence := mustMessage(t, `{"op":3,"d":{"token":"S"}}`)
	noOp := mustMessage(t, `{"d":{"token":"S"}}`)
	nullOp := mustMessage(t, `{"op":null,"d":{"token":"S"}}`)
	stringOp := mustMessage(t, `{"op":"2","d":{"token":"S"}}`)

	lists := discordLists(t)
	for _, tc := range []struct {
		name   string
		lists  [][]Route
		req    Request
		allow  bool
		reason string
	}{
		{"identify", lists, messageRequest(identify, "/d/token"), true, ""},
		{"resume, 6.0 is 6", lists, messageRequest(resume, "/d/token"), true, ""},
		{"resume host", lists, Request{Method: "GET", Host: "us-east1-b.discord.gg", URL: "/", Message: resume, At: []string{"/d/token"}}, true, ""},
		{"presence", lists, messageRequest(presence, "/d/token"), false, "only in messages"},
		{"missing op", lists, messageRequest(noOp, "/d/token"), false, "only in messages"},
		{"null op", lists, messageRequest(nullOp, "/d/token"), false, "only in messages"},
		{"string op", lists, messageRequest(stringOp, "/d/token"), false, "only in messages"},
		{"another place", lists, messageRequest(identify, "/d/intents"), false, "does not name the message place /d/intents"},
		{"another host", lists, Request{Method: "GET", Host: "discord.com", URL: "/", Message: identify, At: []string{"/d/token"}}, false, "no route"},
		{"a POST", lists, Request{Method: "POST", Host: "gateway.discord.gg", URL: "/", Message: identify, At: []string{"/d/token"}}, false, "does not allow POST"},
		{"any host is no message place", [][]Route{{mustParse(t, "*")}}, messageRequest(identify, "/d/token"), false, "names no message place"},
		{"a body route is no message place", [][]Route{{mustParse(t, "* body=/d/token")}}, messageRequest(identify, "/d/token"), false, "no route"},
		{"a list without the block refuses", append(discordLists(t), []Route{mustParse(t, "*.discord.gg")}), messageRequest(identify, "/d/token"), false, "names no message place"},
		{"every list with a block allows", append(discordLists(t), []Route{mustParse(t, `gateway.discord.gg ws=[{"at":"/d/token"}]`)}), messageRequest(identify, "/d/token"), true, ""},
		{"a narrower list refuses resume", append(discordLists(t), []Route{mustParse(t, `gateway.discord.gg ws=[{"at":"/d/token","when":{"op":2}}]`)}), messageRequest(resume, "/d/token"), false, "only in messages"},
		{"a host-wide block refuses", [][]Route{{mustParse(t, discordRoute), mustParse(t, "gateway.discord.gg=blocked")}}, messageRequest(identify, "/d/token"), false, "refuses the message place"},
		{"a blocked place refuses", [][]Route{{mustParse(t, discordRoute), mustParse(t, `gateway.discord.gg ws=[{"at":"/d/token","when":{"op":2}}]=blocked`)}}, messageRequest(identify, "/d/token"), false, "refuses"},
		{"a blocked place elsewhere does not", [][]Route{{mustParse(t, discordRoute), mustParse(t, `gateway.discord.gg ws=[{"at":"/d/token","when":{"op":3}}]=blocked`)}}, messageRequest(identify, "/d/token"), true, ""},
		{"a more specific open route wins over a wider block", [][]Route{{mustParse(t, discordRoute), mustParse(t, "*.discord.gg=blocked")}}, messageRequest(identify, "/d/token"), true, ""},
		{"a blocked body route is no carve-out for messages", [][]Route{{mustParse(t, discordRoute), mustParse(t, "gateway.discord.gg body=/d/token=blocked")}}, messageRequest(identify, "/d/token"), true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allowed, reason := Decide(tc.lists, tc.req)
			if allowed != tc.allow || !strings.Contains(reason, tc.reason) {
				t.Fatalf("Decide = %v, %q; want %v, %q", allowed, reason, tc.allow, tc.reason)
			}
		})
	}
}

func TestWebSocketRouteGrantsNoOtherUse(t *testing.T) {
	lists := discordLists(t)
	req := Request{Method: "GET", Host: "gateway.discord.gg", URL: "/"}
	if allowed, _ := Decide(lists, req); allowed {
		t.Fatal("a WebSocket route allowed the credential in the handshake's headers")
	}
	req.At = []string{"/d/token"}
	if allowed, _ := Decide(lists, req); allowed {
		t.Fatal("a WebSocket route allowed a body place")
	}
	// Nor does a message with no use decide anything.
	if allowed, _ := Decide(lists, messageRequest(mustMessage(t, `{"op":2}`))); allowed {
		t.Fatal("a message with no use was allowed")
	}
	// A blocked WebSocket route is no carve-out for the headers.
	open := [][]Route{{mustParse(t, "gateway.discord.gg"), mustParse(t, `gateway.discord.gg ws=[{"at":"/d/token"}]=blocked`)}}
	if allowed, reason := Decide(open, Request{Method: "GET", Host: "gateway.discord.gg", URL: "/"}); !allowed {
		t.Fatalf("headers refused: %s", reason)
	}
}

// One grant must allow every place a sentinel stands at in one message: two
// grants each allowing one allow neither together (ADR 26-11-03-329 §3).
func TestDecideGrantsMessageNeedsOneGrant(t *testing.T) {
	m := mustMessage(t, `{"op":2,"a":"S","b":"S"}`)
	onlyA := Grant{Lists: [][]Route{{mustParse(t, `gateway.discord.gg ws=[{"at":"/a"}]`)}}}
	onlyB := Grant{Lists: [][]Route{{mustParse(t, `gateway.discord.gg ws=[{"at":"/b"}]`)}}}
	now := time.Now()
	if allowed, _ := DecideGrants([]Grant{onlyA, onlyB}, now, messageRequest(m, "/a", "/b")); allowed {
		t.Fatal("two partial grants allowed together")
	}
	later := now.Add(time.Hour)
	both := Grant{Lists: [][]Route{{mustParse(t, `gateway.discord.gg ws=[{"at":"/a"},{"at":"/b"}]`)}}, ExpiresAt: &later}
	if allowed, reason := DecideGrants([]Grant{onlyA, onlyB, both}, now, messageRequest(m, "/a", "/b")); !allowed {
		t.Fatalf("a complete grant refused: %s", reason)
	}
	if allowed, _ := DecideGrants([]Grant{onlyA, onlyB, both}, later, messageRequest(m, "/a", "/b")); allowed {
		t.Fatal("an expired complete grant allowed")
	}
}

func TestWebSocketCandidate(t *testing.T) {
	grants := []Grant{{Lists: discordLists(t)}}
	now := time.Now()
	req := Request{Method: "GET", Host: "gateway.discord.gg", URL: "https://gateway.discord.gg/?v=10"}
	if !WebSocketCandidate(grants, now, req) {
		t.Fatal("the gateway is no candidate")
	}
	req.Host = "discord.com"
	if WebSocketCandidate(grants, now, req) {
		t.Fatal("another host is a candidate")
	}
	req.Host = "gateway.discord.gg"
	if WebSocketCandidate([]Grant{{Lists: [][]Route{{mustParse(t, "*")}}}}, now, req) {
		t.Fatal("a route without a block is a candidate")
	}
	past := now.Add(-time.Minute)
	if WebSocketCandidate([]Grant{{Lists: discordLists(t), ExpiresAt: &past}}, now, req) {
		t.Fatal("a lapsed grant is a candidate")
	}
	blocked := mustParse(t, discordRoute)
	blocked.Blocked = true
	if WebSocketCandidate([]Grant{{Lists: [][]Route{{blocked}}}}, now, req) {
		t.Fatal("a blocked route is a candidate")
	}
}

func TestProveContainedMessagePlaces(t *testing.T) {
	parent := []Route{mustParse(t, `gateway.discord.gg ws=[{"at":"/d/token","when":{"op":2}},{"at":"/d/token","when":{"op":6}}]`)}
	for _, tc := range []struct {
		name  string
		grant string
		ok    bool
	}{
		{"same", `gateway.discord.gg ws=[{"at":"/d/token","when":{"op":2}}]`, true},
		{"a condition added narrows", `gateway.discord.gg ws=[{"at":"/d/token","when":{"op":2,"s":null}}]`, true},
		{"2.0 is 2", `gateway.discord.gg ws=[{"at":"/d/token","when":{"op":2.0}}]`, true},
		{"a condition removed widens", `gateway.discord.gg ws=[{"at":"/d/token"}]`, false},
		{"another value", `gateway.discord.gg ws=[{"at":"/d/token","when":{"op":3}}]`, false},
		{"another pointer", `gateway.discord.gg ws=[{"at":"/d/other","when":{"op":2}}]`, false},
		{"no block", `gateway.discord.gg`, false},
		{"another host", `discord.com ws=[{"at":"/d/token","when":{"op":2}}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ProveContained([]Route{mustParse(t, "*")}, []Route{mustParse(t, tc.grant)}, parent)
			if (err == nil) != tc.ok {
				t.Fatalf("ProveContained = %v, want ok %v", err, tc.ok)
			}
		})
	}
	// A parent without a block contains no message grant, even `*`.
	if err := ProveContained([]Route{mustParse(t, "*")}, []Route{mustParse(t, `gateway.discord.gg ws=[{"at":"/d/token"}]`)}, []Route{mustParse(t, "*"), mustParse(t, "a.example")}); err == nil {
		t.Fatal("a parent without a WebSocket block contained a message grant")
	}
}

func TestPlacesAllowedMessagePlaces(t *testing.T) {
	list := []Route{mustParse(t, discordRoute)}
	for _, tc := range []struct {
		grant string
		ok    bool
	}{
		{`gateway.discord.gg ws=[{"at":"/d/token","when":{"op":2}}]`, true},
		{`gateway.discord.gg ws=[{"at":"/d/token"}]`, true}, // overlaps op 2 and op 6
		{`gateway.discord.gg ws=[{"at":"/d/token","when":{"op":3}}]`, false},
		{`gateway.discord.gg ws=[{"at":"/d/other"}]`, false},
		{`discord.com ws=[{"at":"/d/token"}]`, false},
		{`POST gateway.discord.gg ws=[{"at":"/d/token"}]`, false},
	} {
		err := PlacesAllowed([]Route{mustParse(t, tc.grant)}, list)
		if (err == nil) != tc.ok {
			t.Errorf("PlacesAllowed(%s) = %v, want ok %v", tc.grant, err, tc.ok)
		}
	}
	if err := PlacesAllowed([]Route{mustParse(t, `gateway.discord.gg ws=[{"at":"/d/token"}]`)}, []Route{mustParse(t, "*")}); err == nil {
		t.Fatal("`*` allowed a message place")
	}
}

func TestMessagePlaceDescribe(t *testing.T) {
	route := mustParse(t, discordRoute)
	if got := route.WebSocket.Places[0].Describe(); got != `the string at "/d/token" in messages where "op" is 2` {
		t.Fatalf("Describe = %q", got)
	}
	if got := (MessagePlace{At: "/token"}).Describe(); got != `the string at "/token" in any message` {
		t.Fatalf("Describe = %q", got)
	}
	// A name that reads as conditions is shown as the one name it is.
	forged := mustParse(t, `h ws=[{"at":"/d/token","when":{"op is 2 and nonce":"x"}}]`)
	if got := forged.WebSocket.Places[0].Describe(); got != `the string at "/d/token" in messages where "op is 2 and nonce" is "x"` {
		t.Fatalf("Describe = %q", got)
	}
}
