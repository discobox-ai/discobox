package secrets

import (
	"encoding/base64"
	"regexp"
	"strings"
)

// A credential does not always travel as itself. Git's HTTP transport sends a
// username and password as `Authorization: Basic base64(user:password)`, so a
// sentinel placed in either half is invisible to a literal scan: the proxy
// swaps nothing and the upstream is handed the placeholder. The same holds for
// anything else that base64-encodes a value the sandbox was given.
//
// So a value is scanned twice: once as itself, and once through each base64
// token in it. A token is decoded, scanned as text, and re-encoded only when a
// sentinel was actually found and resolved — which covers the username half,
// the password half, both halves, and a token that is nothing but an encoded
// sentinel, without this package knowing anything about `user:password` or any
// other structure inside the decoded bytes. A token that does not decode, or
// decodes to text holding no sentinel, is passed through byte-for-byte.

// base64Token matches a run of base64 alphabet characters with optional
// padding. Both alphabets are in the one class; a token that mixes them decodes
// under neither encoding and is skipped. The minimum length is a cheap filter:
// fewer than 8 characters cannot carry an encoded credential.
var base64Token = regexp.MustCompile(`[A-Za-z0-9+/\-_]{8,}={0,2}`)

// maxEncodedToken bounds the decoding a single token can cost. A credential
// encoded into a header is orders of magnitude smaller than this; a token
// larger than it is some other payload that happens to be in the alphabet.
const maxEncodedToken = 64 << 10

// tokenEncoding is the encoding a base64 token arrived in. Only part of that is
// observable, and the two halves are independent:
//
//   - Alphabet. A token containing `-` or `_` is URL-safe; one containing `+` or
//     `/` is standard. A token with none of the four is the same string under
//     both, so this cannot decode wrongly — the alphabet only becomes visible
//     again on re-encoding, and only if the substituted bytes need a character
//     62/63 that the sentinel's did not.
//   - Padding. A token carrying `=` is padded; one whose length is not a
//     multiple of 4 cannot be. A multiple of 4 with no `=` is genuinely
//     ambiguous: it is what a padded encoder emits for a payload that happens to
//     land on a 3-byte boundary, and also what a raw encoder always emits.
//
// Both ambiguities resolve to standard-with-padding, which is what
// `Authorization: Basic` is defined to carry (RFC 7617 over RFC 4648 §4) and so
// what the Git case this exists for needs. Whichever way it resolves, the chosen
// encoding re-encodes the *original* token to itself byte-for-byte, so a token
// that is passed through unchanged is never reshaped. The guess is only visible
// on a token that was rewritten, when the real credential needs a feature the
// sentinel did not: a different length picks up `=`, different bytes pick up `+`
// or `/`. That is still valid base64 of the right credential, and a consumer
// insisting on the raw or URL-safe spelling rejects it loudly rather than
// receiving a wrong credential quietly.
func tokenEncoding(token string) *base64.Encoding {
	urlSafe := strings.ContainsAny(token, "-_")
	raw := len(token)%4 != 0
	switch {
	case urlSafe && raw:
		return base64.RawURLEncoding
	case urlSafe:
		return base64.URLEncoding
	case raw:
		return base64.RawStdEncoding
	default:
		return base64.StdEncoding
	}
}

// swapEncoded rewrites every base64 token in value whose decoded text holds a
// resolvable sentinel, leaving the rest of value untouched.
func swapEncoded(value string, sentinels []string, lookup lookupFunc) (string, bool) {
	matches := base64Token.FindAllStringIndex(value, -1)
	if len(matches) == 0 {
		return value, false
	}
	var out strings.Builder
	end := 0
	for _, match := range matches {
		token := value[match[0]:match[1]]
		if len(token) > maxEncodedToken {
			continue
		}
		swapped, ok := swapEncodedToken(token, sentinels, lookup)
		if !ok {
			continue
		}
		out.WriteString(value[end:match[0]])
		out.WriteString(swapped)
		end = match[1]
	}
	if end == 0 {
		return value, false
	}
	out.WriteString(value[end:])
	return out.String(), true
}

// swapEncodedToken decodes one base64 token and re-encodes it with its
// sentinels substituted. It reports false when the token does not decode or
// carries nothing to swap.
//
// Decoding is strict, so a token is only rewritten when its bytes are the
// canonical encoding of what comes back out. A sloppy encoder's token is left
// alone, which sends the sentinel upstream and fails the request rather than
// re-spelling someone else's payload.
func swapEncodedToken(token string, sentinels []string, lookup lookupFunc) (string, bool) {
	encoding := tokenEncoding(token)
	decoded, err := encoding.Strict().DecodeString(token)
	if err != nil {
		return "", false
	}
	swapped, ok := replaceSentinels(string(decoded), sentinels, lookup)
	if !ok {
		return "", false
	}
	return encoding.EncodeToString([]byte(swapped)), true
}
