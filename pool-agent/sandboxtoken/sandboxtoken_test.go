package sandboxtoken

import (
	"crypto/ed25519"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"

	"github.com/discobox-ai/discobox/pool-agent/poolauth"
)

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return public, private
}

func testClaims() Claims {
	return Claims{ProjectID: "project-1", PoolID: "pool-1", SandboxID: "sandbox-1", Scopes: []string{ScopeOriginFetch}}
}

func TestATokenVerifiesAgainstTheKeyThatIssuedIt(t *testing.T) {
	public, private := testKey(t)
	token, err := Issue(private, testClaims(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(public)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := verifier.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.SandboxID != "sandbox-1" || claims.ProjectID != "project-1" || claims.PoolID != "pool-1" || len(claims.Scopes) != 1 || claims.Scopes[0] != ScopeOriginFetch {
		t.Fatalf("claims = %+v", claims)
	}

	other, _ := testKey(t)
	otherVerifier, err := NewVerifier(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherVerifier.Verify(token); err == nil {
		t.Fatal("a token verified against a key that did not issue it")
	}
}

// The pool signs its assertions to the control plane with the same key, so
// the audience is all that keeps one from passing for the other.
func TestASandboxTokenAndAPoolAssertionAreNotInterchangeable(t *testing.T) {
	public, private := testKey(t)
	assertion, err := poolauth.CreateToken(private, poolauth.Claims{ProjectID: "project-1", PoolID: "pool-1", Scopes: []string{ScopeOriginFetch}})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(public)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(assertion); err == nil {
		t.Fatal("a pool assertion passed for a sandbox token")
	}

	token, err := Issue(private, testClaims(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := poolauth.EncodePublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := poolauth.VerifyToken(encoded, token); err == nil {
		t.Fatal("a sandbox token passed for a pool assertion")
	}
}

func TestATokenNamesItsSandbox(t *testing.T) {
	_, private := testKey(t)
	claims := testClaims()
	claims.SandboxID = ""
	if _, err := Issue(private, claims, time.Hour); err == nil {
		t.Fatal("issued a token for no sandbox")
	}
}

func TestAnExpiredTokenIsRefused(t *testing.T) {
	public, private := testKey(t)
	token, err := Issue(private, testClaims(), time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	verifier, err := NewVerifier(public)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(token); err == nil {
		t.Fatal("an expired token verified")
	}
}

// The routes that accept a sandbox token read its scopes the way they read the
// control plane's, where "*" or sandbox:write is a push. So no other scope is
// ever issued, and a token carrying one is refused even with a good signature.
func TestASandboxTokenCarriesOnlyOriginFetch(t *testing.T) {
	public, private := testKey(t)
	for _, scope := range []string{"*", "sandbox:write", "sandbox:*"} {
		claims := testClaims()
		claims.Scopes = []string{ScopeOriginFetch, scope}
		if _, err := Issue(private, claims, time.Hour); err == nil {
			t.Errorf("issued a token carrying %q", scope)
		}
	}

	secretKey, err := paseto.NewV4AsymmetricSecretKeyFromEd25519(private)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	token := paseto.NewToken()
	token.SetAudience(Audience)
	token.SetIssuedAt(now)
	token.SetNotBefore(now)
	token.SetExpiration(now.Add(time.Hour))
	token.SetString("project_id", "project-1")
	token.SetString("pool_id", "pool-1")
	token.SetString("sandbox_id", "sandbox-1")
	if err := token.Set("scopes", []string{ScopeOriginFetch, "*"}); err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(public)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(token.V4Sign(secretKey, nil)); err == nil {
		t.Fatal("verified a sandbox token carrying \"*\"")
	}
}
