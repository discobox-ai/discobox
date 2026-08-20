package endpoint

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
)

func TestParseIrohIDRoundTripsHex(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	want, err := IrohIDFromPublicKey(pub)
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey() error = %v", err)
	}
	got, err := ParseIrohID(want.String())
	if err != nil {
		t.Fatalf("ParseIrohID() error = %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %s, want %s", got, want)
	}
	if len(want.String()) != 2*IrohIDSize {
		t.Fatalf("text form is %d chars, want %d", len(want.String()), 2*IrohIDSize)
	}
}

// A truncated ID is a different identity rather than a prefix of this one, so
// accepting one would let a mistyped address resolve to nobody instead of
// failing where it was typed.
func TestParseIrohIDRejectsMalformed(t *testing.T) {
	full := strings.Repeat("ab", IrohIDSize)
	for name, value := range map[string]string{
		"empty":     "",
		"blank":     "   ",
		"truncated": full[:len(full)-2],
		"too long":  full + "cd",
		"not hex":   strings.Repeat("zz", IrohIDSize),
	} {
		if _, err := ParseIrohID(value); err == nil {
			t.Fatalf("ParseIrohID(%s) succeeded, want error", name)
		}
	}
}

func TestParseIrohIDNormalizesCaseAndSpace(t *testing.T) {
	full := strings.Repeat("AB", IrohIDSize)
	id, err := ParseIrohID("  " + full + "  ")
	if err != nil {
		t.Fatalf("ParseIrohID() error = %v", err)
	}
	if got, want := id.String(), strings.ToLower(full); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestParseDialFormCarriesEndpointID(t *testing.T) {
	full := strings.Repeat("ab", IrohIDSize)
	parsed, err := Parse("iroh://" + full)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if parsed.Scheme != "iroh" {
		t.Fatalf("Scheme = %q, want iroh", parsed.Scheme)
	}
	id, err := parsed.IrohID()
	if err != nil {
		t.Fatalf("IrohID() error = %v", err)
	}
	if id.String() != full {
		t.Fatalf("IrohID() = %s, want %s", id, full)
	}
}

// "iroh://" is the listen form: a server's identity comes from its key file, so
// there is no address to configure and none to dial.
func TestParseListenFormNamesNoPeer(t *testing.T) {
	parsed, err := Parse("iroh://")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if parsed.Scheme != "iroh" {
		t.Fatalf("Scheme = %q, want iroh", parsed.Scheme)
	}
	if _, err := parsed.IrohID(); err == nil {
		t.Fatal("IrohID() succeeded on the listen form, want error")
	}
}

func TestParseRejectsMalformedIrohEndpoint(t *testing.T) {
	for _, raw := range []string{"iroh://not-an-id", "iroh:///some/path", "iroh://" + strings.Repeat("ab", 8)} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("Parse(%q) succeeded, want error", raw)
		}
	}
}

// An iroh endpoint is reached over the network and its server belongs to
// whoever runs it, so it is never auto-launched; and git cannot dial it, so it
// always needs the loopback bridge. Those two answers are what the capability
// methods exist to keep apart.
func TestIrohEndpointCapabilities(t *testing.T) {
	parsed, err := Parse("iroh://" + strings.Repeat("ab", IrohIDSize))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if parsed.AutoLaunchable() {
		t.Fatal("AutoLaunchable() = true, want false")
	}
	if parsed.DirectlyDialable() {
		t.Fatal("DirectlyDialable() = true, want false")
	}
}

// Reaching an iroh endpoint needs an identity, and this process has none until
// [ConfigureIroh] installs one. The failure has to name that rather than the
// scheme, which is understood perfectly well.
func TestIrohEndpointRequiresAnIdentity(t *testing.T) {
	raw := "iroh://" + strings.Repeat("ab", IrohIDSize)
	_, _, err := HTTPClient(raw, nil)
	if err == nil {
		t.Fatal("HTTPClient() succeeded without an iroh identity configured")
	}
	if strings.Contains(err.Error(), "unsupported endpoint scheme") {
		t.Fatalf("HTTPClient() error = %v, want it to name the missing identity", err)
	}
	if !errors.Is(err, errIrohNotConfigured) {
		t.Fatalf("HTTPClient() error = %v, want errIrohNotConfigured", err)
	}
}

// An endpoint ID is not routable on its own, so the URL has to be able to carry
// the addresses that reach it until a discovery service exists.
func TestParseCarriesDirectAddresses(t *testing.T) {
	full := strings.Repeat("ab", IrohIDSize)
	parsed, err := Parse("iroh://" + full + "?addr=127.0.0.1%3A41234&addr=%5B%3A%3A1%5D%3A41235")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	id, err := parsed.IrohID()
	if err != nil {
		t.Fatalf("IrohID() error = %v", err)
	}
	if id.String() != full {
		t.Fatalf("IrohID() = %s, want %s", id, full)
	}
	want := []string{"127.0.0.1:41234", "[::1]:41235"}
	if len(parsed.IrohAddrs) != len(want) {
		t.Fatalf("IrohAddrs = %v, want %v", parsed.IrohAddrs, want)
	}
	for i, addr := range want {
		if parsed.IrohAddrs[i] != addr {
			t.Fatalf("IrohAddrs[%d] = %q, want %q", i, parsed.IrohAddrs[i], addr)
		}
	}
}

func TestIrohURLWithAddrsRoundTrips(t *testing.T) {
	id, err := ParseIrohID(strings.Repeat("ef", IrohIDSize))
	if err != nil {
		t.Fatalf("ParseIrohID() error = %v", err)
	}
	addrs := []string{"127.0.0.1:41234", "[::1]:41235"}
	parsed, err := Parse(IrohURLWithAddrs(id, addrs))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	got, err := parsed.IrohID()
	if err != nil {
		t.Fatalf("IrohID() error = %v", err)
	}
	if got != id {
		t.Fatalf("id round trip = %s, want %s", got, id)
	}
	if len(parsed.IrohAddrs) != len(addrs) {
		t.Fatalf("IrohAddrs = %v, want %v", parsed.IrohAddrs, addrs)
	}
}

func TestIrohURLDialsTheID(t *testing.T) {
	id, err := ParseIrohID(strings.Repeat("cd", IrohIDSize))
	if err != nil {
		t.Fatalf("ParseIrohID() error = %v", err)
	}
	parsed, err := Parse(IrohURL(id))
	if err != nil {
		t.Fatalf("Parse(IrohURL()) error = %v", err)
	}
	got, err := parsed.IrohID()
	if err != nil {
		t.Fatalf("IrohID() error = %v", err)
	}
	if got != id {
		t.Fatalf("round trip = %s, want %s", got, id)
	}
}
