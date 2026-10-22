// Package secretformat describes the shape of credential values so the system
// can generate convincing sentinel placeholders, validate inline values, and
// infer a format from a real value.
//
// A format is a small generative template: literal text interleaved with
// {charset:length} tokens, for example:
//
//	sk-ant-oat01-{base64url:93}
//	ghp_{base62:36}
//	{hex:64}
//
// Generating fills each token with cryptographically-random characters from its
// charset, producing a value byte-shape-identical to a real credential. The same
// template compiles to an anchored regular expression for validation.
package secretformat

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// charsets maps a template charset name to its alphabet.
var charsets = map[string]string{
	"digits":    "0123456789",
	"hex":       "0123456789abcdef",
	"HEX":       "0123456789ABCDEF",
	"lower":     "abcdefghijklmnopqrstuvwxyz",
	"upper":     "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	"alnum":     "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ",
	"base62":    "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ",
	"base32":    "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567",
	"base64url": "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-_",
	"base64":    "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ+/",
}

// classifyOrder lists charsets from tightest to loosest for inference. The first
// charset whose alphabet is a superset of a segment's characters is chosen.
var classifyOrder = []string{"digits", "hex", "HEX", "lower", "upper", "base32", "alnum", "base64url", "base64"}

var tokenPattern = regexp.MustCompile(`^\{([A-Za-z0-9]+):(\d+)\}`)

type part struct {
	literal string // set when this part is literal text
	charset string // set when this part is a random token
	length  int
}

// Template is a parsed secret format.
type Template struct {
	raw   string
	parts []part
}

// MaxLength bounds the value a template a person wrote generates (ParseChosen).
// It is not a bound on credentials: an inferred template is as long as the
// value it was read from, and a long JWT or cloud token must still mint a
// sentinel of its own shape rather than fail to parse and fall back to the
// default one.
const MaxLength = 4096

// Parse compiles a format template.
func Parse(format string) (*Template, error) {
	if strings.TrimSpace(format) == "" {
		return nil, fmt.Errorf("format is empty")
	}
	var parts []part
	var literal strings.Builder
	rest := format
	for len(rest) > 0 {
		if rest[0] == '{' {
			match := tokenPattern.FindStringSubmatch(rest)
			if match == nil {
				return nil, fmt.Errorf("invalid format token at %q", rest)
			}
			name := match[1]
			if _, ok := charsets[name]; !ok {
				return nil, fmt.Errorf("unknown charset %q", name)
			}
			length, err := strconv.Atoi(match[2])
			if err != nil || length <= 0 {
				return nil, fmt.Errorf("invalid token length in %q", match[0])
			}
			if literal.Len() > 0 {
				parts = append(parts, part{literal: literal.String()})
				literal.Reset()
			}
			parts = append(parts, part{charset: name, length: length})
			rest = rest[len(match[0]):]
			continue
		}
		literal.WriteByte(rest[0])
		rest = rest[1:]
	}
	if literal.Len() > 0 {
		parts = append(parts, part{literal: literal.String()})
	}
	return &Template{raw: format, parts: parts}, nil
}

// ParseChosen compiles a template a person wrote, which is also held to
// MaxLength: every sentinel minted for the secret generates it, and nobody
// means a template that generates megabytes.
func ParseChosen(format string) (*Template, error) {
	t, err := Parse(format)
	if err != nil {
		return nil, err
	}
	total := 0
	for _, p := range t.parts {
		total += len(p.literal) + p.length
	}
	if total > MaxLength {
		return nil, fmt.Errorf("format generates %d characters, more than %d", total, MaxLength)
	}
	return t, nil
}

// String returns the raw format string.
func (t *Template) String() string { return t.raw }

// Generate produces a random value matching the template.
func (t *Template) Generate() (string, error) {
	var out strings.Builder
	for _, p := range t.parts {
		if p.charset == "" {
			out.WriteString(p.literal)
			continue
		}
		alphabet := charsets[p.charset]
		for i := 0; i < p.length; i++ {
			idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
			if err != nil {
				return "", err
			}
			out.WriteByte(alphabet[idx.Int64()])
		}
	}
	return out.String(), nil
}

// Validate reports whether value matches the template's shape.
func (t *Template) Validate(value string) bool {
	// Walked part by part rather than compiled to a regular expression: RE2
	// refuses a repeat count over 1000, and a template read from a long
	// credential has segments far longer than that.
	for _, p := range t.parts {
		if p.charset == "" {
			rest, ok := strings.CutPrefix(value, p.literal)
			if !ok {
				return false
			}
			value = rest
			continue
		}
		if len(value) < p.length || !coversAll(charsets[p.charset], value[:p.length]) {
			return false
		}
		value = value[p.length:]
	}
	return value == ""
}

// DefaultSentinelFormat is the shape used when a secret has no format of its
// own. It is opaque on purpose: a sentinel that mimics nothing is still a valid
// sentinel, since detection is exact-set matching rather than parsing.
const DefaultSentinelFormat = "{alnum:48}"

// MintSentinel generates one sentinel placeholder from a secret's format,
// falling back to DefaultSentinelFormat when the format is empty or
// unparseable. Every sentinel in the system — the stable one bound to a sandbox
// env var, and the ephemeral one a pool agent mints per use — comes from here,
// so the two can never be shaped by different rules.
func MintSentinel(format string) (string, error) {
	tmpl, err := Parse(strings.TrimSpace(format))
	if err != nil {
		tmpl, err = Parse(DefaultSentinelFormat)
		if err != nil {
			return "", err
		}
	}
	return tmpl.Generate()
}
