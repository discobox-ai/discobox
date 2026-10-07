package discobot_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"

	"github.com/discobox-ai/discobox/server/internal/auth/discobot"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return public, private
}

func TestAnAssertionSaysWhoAndWhere(t *testing.T) {
	public, private := newKey(t)
	token, err := discobot.Sign(private, discobot.Claims{UserID: "usr_priya", Name: "Priya Raman", ProjectID: "proj_acme", ID: "a1"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := discobot.Verify(public, token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims != (discobot.Claims{UserID: "usr_priya", Name: "Priya Raman", ProjectID: "proj_acme", ID: "a1"}) {
		t.Fatalf("claims = %#v", claims)
	}
}

func TestAnAssertionWithoutAProjectActsInNone(t *testing.T) {
	public, private := newKey(t)
	token, err := discobot.Sign(private, discobot.Claims{UserID: "usr_priya", ID: "a1"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := discobot.Verify(public, token)
	if err != nil || claims.ProjectID != "" {
		t.Fatalf("claims = %#v, err = %v", claims, err)
	}
}

func TestAnAssertionIsRefused(t *testing.T) {
	public, private := newKey(t)
	other, _ := newKey(t)
	signed := func(edit func(*paseto.Token)) string {
		key, err := paseto.NewV4AsymmetricSecretKeyFromEd25519(private)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		token := paseto.NewToken()
		token.SetIssuer(discobot.Issuer)
		token.SetAudience(discobot.Audience)
		token.SetSubject("usr_priya")
		token.SetJti("a1")
		token.SetIssuedAt(now)
		token.SetNotBefore(now)
		token.SetExpiration(now.Add(time.Minute))
		edit(&token)
		return token.V4Sign(key, nil)
	}
	valid := signed(func(*paseto.Token) {})
	for _, tc := range []struct {
		name  string
		key   ed25519.PublicKey
		token string
		want  string
	}{
		{"signed by another key", other, valid, "bad signature"},
		{"for another audience", public, signed(func(t *paseto.Token) { t.SetAudience("someone-else") }), "not intended for"},
		{"from another issuer", public, signed(func(t *paseto.Token) { t.SetIssuer("someone-else") }), "not issued by"},
		{"expired", public, signed(func(t *paseto.Token) { t.SetExpiration(time.Now().Add(-time.Second)) }), "expires"},
		{"good for too long", public, signed(func(t *paseto.Token) { t.SetExpiration(time.Now().Add(time.Hour)) }), "more than"},
		{"naming no person", public, signed(func(t *paseto.Token) { t.SetSubject("") }), "names no person"},
		{"without an ID", public, signed(func(t *paseto.Token) { t.SetJti("") }), "no ID"},
		{"not a token", public, "v4.public.nonsense", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := discobot.Verify(tc.key, tc.token)
			if err == nil {
				t.Fatal("accepted")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestThePublicKeyIsRawBase64(t *testing.T) {
	public, _ := newKey(t)
	parsed, err := discobot.ParsePublicKey(base64.StdEncoding.EncodeToString(public))
	if err != nil || !parsed.Equal(public) {
		t.Fatalf("parsed = %x, err = %v", parsed, err)
	}
	if _, err := discobot.ParsePublicKey(base64.StdEncoding.EncodeToString(public[:16])); err == nil {
		t.Fatal("accepted a short key")
	}
}
