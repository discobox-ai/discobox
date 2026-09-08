package endpoint

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

func testPeerID(t *testing.T) IrohID {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	id, err := IrohIDFromPublicKey(pub)
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey() error = %v", err)
	}
	return id
}

// Every peer ID has to survive the trip out to a human and back, whatever they
// did to it on the way.
func TestPeerIDRoundTrips(t *testing.T) {
	for range 500 {
		id := testPeerID(t)
		for _, form := range []string{
			id.String(),                  // as printed
			id.Key(),                     // as stored
			strings.ToUpper(id.String()), // typed in caps
			strings.ReplaceAll(id.String(), "-", "--"), // mangled hyphens
			" " + id.String() + " ",                    // pasted with whitespace
		} {
			back, err := ParseIrohID(form)
			if err != nil {
				t.Fatalf("ParseIrohID(%q) error = %v", form, err)
			}
			if back != id {
				t.Fatalf("ParseIrohID(%q) returned a different ID", form)
			}
		}
	}
}

// The display form is what the ADR promises an operator sees.
func TestPeerIDDisplayShape(t *testing.T) {
	id := testPeerID(t)
	display, key := id.String(), id.Key()

	if !strings.HasPrefix(display, "d1-") {
		t.Fatalf("display form %q does not start with the version", display)
	}
	if got, want := len(display), 66; got != want {
		t.Fatalf("display form is %d characters, want %d", got, want)
	}
	if got, want := len(key), 58; got != want {
		t.Fatalf("stored form is %d characters, want %d", got, want)
	}
	if strings.Contains(key, "-") {
		t.Fatalf("stored form %q contains dashes; a prefix match needs one stable string", key)
	}
	if display != strings.ToLower(display) {
		t.Fatalf("display form %q is not lowercase", display)
	}
	for _, group := range strings.Split(strings.TrimPrefix(display, "d1-"), "-") {
		if len(group) != 7 {
			t.Fatalf("display form %q has a group of %d, want groups of 7", display, len(group))
		}
	}
}

// The check symbols are the reason this format costs more characters than hex:
// a mistyped ID fails here rather than becoming a connection that never works.
func TestPeerIDCatchesTypos(t *testing.T) {
	id := testPeerID(t)
	key := id.Key()

	// Every single-symbol substitution in the body is rejected.
	substitutions := 0
	for i := len(PeerIDVersion); i < len(key); i++ {
		for _, replacement := range []byte{'0', '1', 'z', 'q'} {
			if key[i] == replacement {
				continue
			}
			mutated := key[:i] + string(replacement) + key[i+1:]
			if _, err := ParseIrohID(mutated); err == nil {
				t.Fatalf("ParseIrohID accepted %q, one symbol away from a real ID", mutated)
			}
			substitutions++
		}
	}
	if substitutions == 0 {
		t.Fatal("no substitutions were tried")
	}

	// And transpositions, which is what a check symbol is really for. Luhn
	// mod-32 catches every adjacent pair except "0" with "z" — the analog of
	// mod-10 Luhn's blind spot on 0 and 9, inherent to the algorithm — so that
	// one pair is skipped rather than pretended away.
	transpositions := 0
	for i := len(PeerIDVersion); i < len(key)-1; i++ {
		a, b := key[i], key[i+1]
		if a == b || (a == '0' && b == 'z') || (a == 'z' && b == '0') {
			continue
		}
		mutated := key[:i] + string(b) + string(a) + key[i+2:]
		if _, err := ParseIrohID(mutated); err == nil {
			t.Fatalf("ParseIrohID accepted %q, a transposition of a real ID", mutated)
		}
		transpositions++
	}
	if transpositions == 0 {
		t.Fatal("no transpositions were tried")
	}
}

// The one transposition Luhn mod-32 cannot see, asserted so that a future
// change of check algorithm is noticed here rather than believed to be
// unnecessary.
func TestPeerIDTranspositionBlindSpotIsOnlyZeroAndZ(t *testing.T) {
	body := strings.Repeat("0", peerIDGroupLen)
	for a := range CrockfordAlphabet {
		for b := range CrockfordAlphabet {
			if a == b {
				continue
			}
			first := body[:5] + string(CrockfordAlphabet[a]) + string(CrockfordAlphabet[b]) + body[7:]
			swapped := body[:5] + string(CrockfordAlphabet[b]) + string(CrockfordAlphabet[a]) + body[7:]
			collides := luhnCheck(first) == luhnCheck(swapped)
			zeroAndZ := (CrockfordAlphabet[a] == '0' && CrockfordAlphabet[b] == 'z') ||
				(CrockfordAlphabet[a] == 'z' && CrockfordAlphabet[b] == '0')
			if collides != zeroAndZ {
				t.Fatalf("transposing %q and %q: collides=%v, want %v",
					string(CrockfordAlphabet[a]), string(CrockfordAlphabet[b]), collides, zeroAndZ)
			}
		}
	}
}

// Crockford's confusable mappings mean a human who reads 1 as l, or 0 as O,
// still names the right peer.
func TestPeerIDAcceptsConfusableCharacters(t *testing.T) {
	id := testPeerID(t)
	typed := strings.NewReplacer("1", "l", "0", "O").Replace(id.String())
	back, err := ParseIrohID(typed)
	if err != nil {
		t.Fatalf("ParseIrohID(%q) error = %v", typed, err)
	}
	if back != id {
		t.Fatalf("confusable characters resolved to a different ID")
	}
}

// Hex is not a peer ID any more, and saying so is the whole point of enforcing
// one form (ADR 0097 §6).
func TestPeerIDRejectsHexAndOtherShapes(t *testing.T) {
	for _, value := range []string{
		"",
		"   ",
		"6ebfa69c6f71c5f4948a0ac6eabe40ec5bc261cb799d61462e5aaf4421cee2ec",
		"d1",
		"d1-dtztd73",
		"d2-dtztd73-fe72z98-54a1b3e-nfj0xhx-dw4rebf-6ep2hhn-ebaqm88-eewbp0w",
		"d1-dtztd73-fe72z95-54a1b3e-nfj0xh3-dw4rebf-6ep2hhn-ebaqm88-eewbp0vx",
		"d1-utztd73-fe72z98-54a1b3e-nfj0xhx-dw4rebf-6ep2hhn-ebaqm88-eewbp0w",
	} {
		if _, err := ParseIrohID(value); err == nil {
			t.Fatalf("ParseIrohID(%q) succeeded, want a rejection", value)
		}
	}
}

// The example every error shows must itself be a peer ID.
//
// It is what `ParseIrohID` prints as "a peer ID looks like", what the package
// doc shows, and what LogAuthorizedIDProblems hands an operator whose
// authorized_ids just stopped working (ADR 0097 §6). An invalid one tells that
// operator their correct ID is mistyped — and the first version of this
// constant was exactly that, computed by hand before luhnCheck was corrected,
// with three of its four check symbols wrong. Nothing caught it because the
// only test that mentioned it grepped an error string for it.
func TestExamplePeerIDIsAPeerID(t *testing.T) {
	id, err := ParseIrohID(ExamplePeerID)
	if err != nil {
		t.Fatalf("ExamplePeerID does not parse: %v", err)
	}
	if id.String() != ExamplePeerID {
		t.Fatalf("ExamplePeerID is not the form String renders:\n got %s\nwant %s", id.String(), ExamplePeerID)
	}
}

// An error has to show the shape, because a symbol count is not something a
// reader can act on.
func TestPeerIDErrorsShowTheShape(t *testing.T) {
	_, err := ParseIrohID("6ebfa69c6f71c5f4948a0ac6eabe40ec5bc261cb799d61462e5aaf4421cee2ec")
	if err == nil {
		t.Fatal("hex was accepted")
	}
	if !strings.Contains(err.Error(), ExamplePeerID) {
		t.Fatalf("error %q does not show what a peer ID looks like", err)
	}
}

// Short is for a log line and must never be mistaken for the real thing.
func TestPeerIDShortIsNotDialable(t *testing.T) {
	id := testPeerID(t)
	if _, err := ParseIrohID(id.Short()); err == nil {
		t.Fatalf("the short form %q parsed as a peer ID", id.Short())
	}
}
