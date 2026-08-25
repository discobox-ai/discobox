package poolauth

import (
	"crypto/ed25519"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"
)

// A pool VM steps its clock off the host RTC every 30s and drifts either side
// of it in between, so an assertion is routinely minted a second or two ahead
// of the control plane's clock.
//
// NotBefore was backdated by ClockSkew for exactly that, and IssuedAt was not —
// which made the allowance no allowance at all. The verifier checks both, and
// rejected the token with "the ValidAt time is before this token was issued".
// The pool agent exited on that 401, so the pool never came up.
func TestAssertionSurvivesAnIssuerClockAheadOfTheVerifier(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodePublicKey(public)
	if err != nil {
		t.Fatal(err)
	}

	token, err := CreateTokenWithTTL(private, Claims{ProjectID: "proj_1", PoolID: "pool_1"}, TokenTTL)
	if err != nil {
		t.Fatal(err)
	}

	// Verify as a control plane whose clock is two seconds behind the issuer's,
	// which is what "the guest is slightly ahead" looks like from this side.
	parser := paseto.NewParser()
	parser.AddRule(paseto.ForAudience(Audience))
	parser.AddRule(paseto.ValidAt(time.Now().Add(-2 * time.Second)))
	key, err := paseto.NewV4AsymmetricPublicKeyFromEd25519(public)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseV4Public(key, token, nil); err != nil {
		t.Fatalf("a verifier two seconds behind the issuer rejected the assertion: %v", err)
	}

	// And the ordinary path still works.
	if _, err := VerifyToken(encoded, token); err != nil {
		t.Fatalf("VerifyToken() error = %v", err)
	}
}
