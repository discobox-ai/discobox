package endpoint

import (
	"context"
	"net"
	"reflect"
	"strings"
	"testing"
)

func mustParse(t *testing.T, raw string) Endpoint {
	t.Helper()
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", raw, err)
	}
	return parsed
}

// answerTXT stands in for DNS for the rest of the test. A name missing from
// records is NXDOMAIN, which is what "no record" looks like from a resolver.
// It returns the names that were asked about.
func answerTXT(t *testing.T, records map[string][]string) *[]string {
	t.Helper()
	var asked []string
	previous := lookupTXT
	lookupTXT = func(_ context.Context, name string) ([]string, error) {
		asked = append(asked, name)
		if values, ok := records[name]; ok {
			return values, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	t.Cleanup(func() { lookupTXT = previous })
	return &asked
}

// failTXT makes every lookup fail the way a resolver that never answers does.
func failTXT(t *testing.T) {
	t.Helper()
	previous := lookupTXT
	lookupTXT = func(_ context.Context, name string) ([]string, error) {
		return nil, &net.DNSError{Err: "i/o timeout", Name: name, IsTimeout: true}
	}
	t.Cleanup(func() { lookupTXT = previous })
}

func peerWithKeyByte(t *testing.T, b byte) IrohID {
	t.Helper()
	var key [32]byte
	key[0] = b
	id, err := IrohIDFromPublicKey(key[:])
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey() error = %v", err)
	}
	return id
}

// Every spelling of a server says which transport it means, except a bare
// name, whose DNS decides (ADR 0114 §1).
func TestParseDiscoboxServerForms(t *testing.T) {
	id := testPeerID(t)
	for _, tc := range []struct{ raw, scheme, value string }{
		{"discobox://" + id.String(), "iroh", id.Key()},
		{"discobox://10.0.0.5:8443", "https", "https://10.0.0.5:8443"},
		{"discobox://10.0.0.5", "https", "https://10.0.0.5"},
		{"discobox://[fd00::1]:8443", "https", "https://[fd00::1]:8443"},
		{"discobox://Box.Example.com:8443", "https", "https://box.example.com:8443"},
		{"discobox://Box.Example.com", SchemeDiscobox, "box.example.com"},
		{"discobox://workstation", SchemeDiscobox, "workstation"},
	} {
		got := mustParse(t, tc.raw)
		if got.Scheme != tc.scheme || got.Value != tc.value {
			t.Fatalf("Parse(%q) = %s %q, want %s %q", tc.raw, got.Scheme, got.Value, tc.scheme, tc.value)
		}
		if got.Resolved() == (tc.scheme == SchemeDiscobox) {
			t.Fatalf("Parse(%q).Resolved() = %v", tc.raw, got.Resolved())
		}
		if got.Raw != tc.raw {
			t.Fatalf("Parse(%q).Raw = %q, want what was written", tc.raw, got.Raw)
		}
	}
}

// A host written with the peer-ID prefix is an ID or a mistake. Looking it up
// would put which transport it means in the hands of whoever answers for a
// name nobody meant as one.
func TestParseDiscoboxPeerPrefixIsNeverAName(t *testing.T) {
	asked := answerTXT(t, nil)
	for _, raw := range []string{"discobox://d1-workstation", "discobox://D1-box.example.com", "discobox://d1-abc:443"} {
		if _, err := Resolve(t.Context(), raw); err == nil {
			t.Fatalf("Resolve(%q) succeeded, want the peer ID refused", raw)
		}
	}
	if len(*asked) != 0 {
		t.Fatalf("looked up %v, want nothing asked about a peer ID", *asked)
	}
}

func TestParseDiscoboxRefusesWhatIsNotAServer(t *testing.T) {
	for _, raw := range []string{
		// A discobox, which --server does not take.
		"discobox://box.example.com/sbx_0123",
		// An https server has no peer sockets to carry.
		"discobox://10.0.0.5:8443?addr=10.0.0.5:4433",
		// Not a name DNS could hold a record for.
		"discobox://under_score.example.com",
	} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("Parse(%q) succeeded, want an error", raw)
		}
	}
}

// Only a name is looked up: every other address already said what carries it.
func TestResolveAsksDNSOnlyAboutAName(t *testing.T) {
	asked := answerTXT(t, nil)
	for _, raw := range []string{
		"discobox://" + testPeerID(t).String(),
		"discobox://10.0.0.5",
		"discobox://box.example.com:8443",
		"unix:///tmp/discobox/server.sock",
		"http://127.0.0.1:8080",
	} {
		if _, err := Resolve(t.Context(), raw); err != nil {
			t.Fatalf("Resolve(%q) error = %v", raw, err)
		}
	}
	if len(*asked) != 0 {
		t.Fatalf("looked up %v, want nothing", *asked)
	}
}

func TestResolveNameWithARecordIsThatPeer(t *testing.T) {
	id := testPeerID(t)
	asked := answerTXT(t, map[string][]string{"_discobox.box.example.com": {"  " + id.String() + " "}})

	raw := "discobox://box.example.com?addr=10.0.0.5:4433"
	got, err := Resolve(t.Context(), raw)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	want := Endpoint{Raw: raw, Scheme: "iroh", Value: id.Key(), IrohAddrs: []string{"10.0.0.5:4433"}, Name: "box.example.com"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Resolve() = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(*asked, []string{"_discobox.box.example.com"}) {
		t.Fatalf("looked up %v, want only the _discobox record", *asked)
	}
}

func TestResolveNameWithoutARecordIsHTTPS(t *testing.T) {
	answerTXT(t, nil)
	got, err := Resolve(t.Context(), "discobox://box.example.com")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Scheme != "https" || got.Value != "https://box.example.com" || got.Name != "box.example.com" {
		t.Fatalf("Resolve() = %#v, want https://box.example.com", got)
	}
	if !got.DirectlyDialable() {
		t.Fatal("an https server should be directly dialable")
	}
}

// A lookup that fails is not "no record": dialing https instead would reach
// for a transport the server may not have, and report that instead.
func TestResolveFailedLookupIsAnError(t *testing.T) {
	failTXT(t)
	_, err := Resolve(t.Context(), "discobox://box.example.com")
	if err == nil {
		t.Fatal("Resolve() succeeded on a lookup that failed")
	}
	if !strings.Contains(err.Error(), "_discobox.box.example.com") {
		t.Fatalf("error = %v, want it to name the record", err)
	}
}

// A record somebody published meant a peer, so a record that does not name
// exactly one is refused rather than read as https.
func TestResolveRefusesARecordThatIsNotOnePeer(t *testing.T) {
	a, b := peerWithKeyByte(t, 0xaa), peerWithKeyByte(t, 0xbb)
	for _, values := range [][]string{
		{"v=spf1 -all"},
		{a.String(), b.String()},
	} {
		answerTXT(t, map[string][]string{"_discobox.box.example.com": values})
		if got, err := Resolve(t.Context(), "discobox://box.example.com"); err == nil {
			t.Fatalf("Resolve() with record %q = %#v, want an error", values, got)
		}
	}

	// The same peer twice is one peer.
	answerTXT(t, map[string][]string{"_discobox.box.example.com": {a.String(), a.String()}})
	if _, err := Resolve(t.Context(), "discobox://box.example.com"); err != nil {
		t.Fatalf("Resolve() with one peer published twice error = %v", err)
	}
}

// ?addr= is a peer's sockets, so a name that turns out to be https cannot use
// them, and saying so beats dropping them silently.
func TestResolveRefusesAddrsOnAnHTTPSName(t *testing.T) {
	answerTXT(t, nil)
	if _, err := Resolve(t.Context(), "discobox://box.example.com?addr=10.0.0.5:4433"); err == nil {
		t.Fatal("Resolve() succeeded, want ?addr= on an https name refused")
	}
}

func TestHTTPClientRefusesAnUnresolvedName(t *testing.T) {
	_, _, err := HTTPClient(mustParse(t, "discobox://box.example.com"), nil)
	if err == nil || !strings.Contains(err.Error(), "Resolve") {
		t.Fatalf("HTTPClient() error = %v, want it to say the name needs resolving", err)
	}
}

// A lookup that fails is the address layer's answer, with the way around it.
func TestDiagnoseReportsAFailedLookupAtTheAddress(t *testing.T) {
	failTXT(t)
	diagnosis := Diagnose(t.Context(), "discobox://box.example.com", fastDiagnose())
	failure := diagnosis.FirstFailure()
	if failure == nil || failure.Layer != DiagnosisLayerAddress {
		t.Fatalf("Diagnose() = %+v, want an address-layer failure", diagnosis)
	}
	if !strings.Contains(failure.Hint, "discobox://box.example.com:443") {
		t.Fatalf("hint = %q, want it to offer the lookup-free https form", failure.Hint)
	}
}

func TestParseSandboxAddress(t *testing.T) {
	id := testPeerID(t)
	for _, tc := range []struct{ raw, server, sandbox string }{
		{"discobox://box.example.com/sbx_0123", "discobox://box.example.com", "sbx_0123"},
		{"discobox://10.0.0.5:8443/sbx_0123/", "discobox://10.0.0.5:8443", "sbx_0123"},
		{"discobox://" + id.String() + "/0123?addr=10.0.0.5:4433", "discobox://" + id.String() + "?addr=10.0.0.5:4433", "0123"},
	} {
		got, ok, err := ParseSandboxAddress(tc.raw)
		if err != nil || !ok {
			t.Fatalf("ParseSandboxAddress(%q) = %v, %v", tc.raw, ok, err)
		}
		if got.Server != tc.server || got.Sandbox != tc.sandbox {
			t.Fatalf("ParseSandboxAddress(%q) = %+v, want server %q sandbox %q", tc.raw, got, tc.server, tc.sandbox)
		}
		if _, err := Parse(got.Server); err != nil {
			t.Fatalf("the server half %q does not parse: %v", got.Server, err)
		}
	}

	// What is not written as an address is somebody else's to read.
	for _, raw := range []string{"sbx_0123", "0123", "my-box", ""} {
		if _, ok, err := ParseSandboxAddress(raw); ok || err != nil {
			t.Fatalf("ParseSandboxAddress(%q) = %v, %v, want not an address", raw, ok, err)
		}
	}

	// What is written as one and is not a discobox's address is an error.
	for _, raw := range []string{
		"discobox://box.example.com",
		"discobox://box.example.com/a/b",
		"discobox:///sbx_0123",
		"discobox://d1-typo/sbx_0123",
	} {
		if _, ok, err := ParseSandboxAddress(raw); !ok || err == nil {
			t.Fatalf("ParseSandboxAddress(%q) = %v, %v, want an error", raw, ok, err)
		}
	}
}

// The transport said outright, for a server whose transport no rule infers:
// nothing is looked up, and what comes out is an ordinary http endpoint
// (ADR 0114 §1).
func TestParseDiscoboxTransportSchemes(t *testing.T) {
	asked := answerTXT(t, nil)
	for _, tc := range []struct{ raw, scheme, value string }{
		{"discobox+http://127.0.0.1:8081", "http", "http://127.0.0.1:8081"},
		{"discobox+http://Box.Example.com:8081", "http", "http://box.example.com:8081"},
		{"discobox+http://box.example.com", "http", "http://box.example.com"},
		{"discobox+https://box.example.com:8443", "https", "https://box.example.com:8443"},
	} {
		got := mustParse(t, tc.raw)
		if got.Scheme != tc.scheme || got.Value != tc.value {
			t.Fatalf("Parse(%q) = %s %q, want %s %q", tc.raw, got.Scheme, got.Value, tc.scheme, tc.value)
		}
		if !got.Resolved() || !got.DirectlyDialable() {
			t.Fatalf("Parse(%q) = %#v, want a dialable endpoint", tc.raw, got)
		}
		if _, err := Resolve(t.Context(), tc.raw); err != nil {
			t.Fatalf("Resolve(%q) error = %v", tc.raw, err)
		}
	}
	if len(*asked) != 0 {
		t.Fatalf("looked up %v, want nothing asked about an address that names its transport", *asked)
	}

	for _, raw := range []string{
		"discobox+http://",
		"discobox+http://box.example.com/sbx_0123",
		"discobox+http://box.example.com:8081?addr=10.0.0.5:4433",
	} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("Parse(%q) succeeded, want an error", raw)
		}
	}

	// However it is written, it is one server.
	if a, b := mustParse(t, "discobox+http://127.0.0.1:8081"), mustParse(t, "http://127.0.0.1:8081"); a.Value != b.Value {
		t.Fatalf("%q and %q are different endpoints", a.Value, b.Value)
	}
}

// A discobox's address carries the named transport too, which is what lets one
// be handed out for a server that serves plain http.
func TestParseSandboxAddressWithANamedTransport(t *testing.T) {
	got, ok, err := ParseSandboxAddress("discobox+http://127.0.0.1:8081/sbx_0123")
	if err != nil || !ok {
		t.Fatalf("ParseSandboxAddress() = %v, %v", ok, err)
	}
	if got.Server != "discobox+http://127.0.0.1:8081" || got.Sandbox != "sbx_0123" {
		t.Fatalf("ParseSandboxAddress() = %+v", got)
	}
	if _, err := Parse(got.Server); err != nil {
		t.Fatalf("the server half %q does not parse: %v", got.Server, err)
	}
}
