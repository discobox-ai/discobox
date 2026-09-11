package endpoint

import (
	"errors"
	"strings"
	"testing"

	iroh "github.com/discobox-ai/iroh-go"
)

// Both spellings name the same server, and both resolve to the iroh transport
// so that nothing below Parse has a second scheme to learn (ADR 0097 §2).
func TestParseDialFormCarriesEndpointID(t *testing.T) {
	want := testPeerID(t)
	for _, raw := range []string{
		"discobox://" + want.String(), // the address a user is handed
		"discobox://" + want.Key(),    // the same, with the dashes stripped
		"iroh://" + want.String(),     // the transport-level spelling
	} {
		parsed, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", raw, err)
		}
		if parsed.Scheme != "iroh" {
			t.Fatalf("Parse(%q).Scheme = %q, want iroh", raw, parsed.Scheme)
		}
		id, err := parsed.IrohID()
		if err != nil {
			t.Fatalf("IrohID() error = %v", err)
		}
		if id != want {
			t.Fatalf("Parse(%q) resolved a different peer", raw)
		}
	}
}

// "discobox://" and "iroh://" with no peer are both the listen form: a
// server's identity comes from its key file, so there is no address to
// configure and none to dial.
func TestParseDiscoboxListenForm(t *testing.T) {
	for _, raw := range []string{"discobox://", "iroh://"} {
		parsed, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", raw, err)
		}
		if parsed.Scheme != "iroh" {
			t.Fatalf("Parse(%q).Scheme = %q, want iroh", raw, parsed.Scheme)
		}
		if _, err := parsed.IrohID(); err == nil {
			t.Fatalf("Parse(%q).IrohID() succeeded on the listen form", raw)
		}
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
	for _, raw := range []string{"iroh://not-an-id", "iroh:///some/path", "iroh://d1-dtztd73", "discobox://not-an-id"} {
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
	parsed, err := Parse("iroh://" + testPeerID(t).String())
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
	raw := "iroh://" + testPeerID(t).String()
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

// A server no longer advertises addresses, but the URL still carries them for
// a deployment with no discovery service, or two peers on one host that should
// not wait for a round trip to resolve each other (ADR 0097 §3).
func TestParseCarriesDirectAddresses(t *testing.T) {
	want := testPeerID(t)
	parsed, err := Parse("iroh://" + want.String() + "?addr=127.0.0.1%3A41234&addr=%5B%3A%3A1%5D%3A41235")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	id, err := parsed.IrohID()
	if err != nil {
		t.Fatalf("IrohID() error = %v", err)
	}
	if id != want {
		t.Fatalf("IrohID() = %s, want %s", id, want)
	}
	wantAddrs := []string{"127.0.0.1:41234", "[::1]:41235"}
	if len(parsed.IrohAddrs) != len(wantAddrs) {
		t.Fatalf("IrohAddrs = %v, want %v", parsed.IrohAddrs, wantAddrs)
	}
	for i, addr := range wantAddrs {
		if parsed.IrohAddrs[i] != addr {
			t.Fatalf("IrohAddrs[%d] = %q, want %q", i, parsed.IrohAddrs[i], addr)
		}
	}
}

func TestIrohURLWithAddrsRoundTrips(t *testing.T) {
	id := testPeerID(t)
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
	id := testPeerID(t)
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

// The preset carries relays and discovery together, so a custom relay list has
// to reach the right combination of both (ADR 0096 server config §6).
func TestIrohPresetSelectsCustomRelays(t *testing.T) {
	custom := []string{"https://relay.example"}
	for _, tc := range []struct {
		name      string
		cfg       IrohConfig
		wantPre   iroh.Preset
		wantRelay iroh.RelayMode
	}{
		{"default is n0's relays and discovery", IrohConfig{}, iroh.PresetN0, iroh.RelayFromPreset},
		{"custom relays keep discovery", IrohConfig{RelayURLs: custom}, iroh.PresetN0, iroh.RelayCustom},
		{"custom relays without discovery", IrohConfig{RelayURLs: custom, DisableDiscovery: true}, iroh.PresetMinimal, iroh.RelayCustom},
		// DisableRelay is the more specific request: a caller that wants no
		// relays at all is not asking which ones.
		{"no relays beats a list", IrohConfig{RelayURLs: custom, DisableRelay: true}, iroh.PresetN0NoRelay, iroh.RelayFromPreset},
		{"no relays and no discovery", IrohConfig{DisableRelay: true, DisableDiscovery: true}, iroh.PresetMinimal, iroh.RelayFromPreset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preset, relay := irohPreset(tc.cfg)
			if preset != tc.wantPre || relay != tc.wantRelay {
				t.Fatalf("irohPreset() = (%v, %v), want (%v, %v)", preset, relay, tc.wantPre, tc.wantRelay)
			}
		})
	}
}
