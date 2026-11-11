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
	"math"
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

// charsetOrder is every charset, tightest first, as a person reads the list.
var charsetOrder = []string{"digits", "hex", "HEX", "lower", "upper", "base32", "alnum", "base62", "base64url", "base64"}

// Charset is one charset a format token may name, and its alphabet.
type Charset struct {
	Name, Alphabet string
}

// Charsets is every charset a token may name, tightest first: what a person
// writing a format chooses from.
func Charsets() []Charset {
	out := make([]Charset, 0, len(charsetOrder))
	for _, name := range charsetOrder {
		out = append(out, Charset{Name: name, Alphabet: charsets[name]})
	}
	return out
}

// RandomBits is how many bits of randomness a sentinel minted from t carries:
// each token's length times log2 of its alphabet's size. Literals carry none.
func (t *Template) RandomBits() float64 {
	bits := 0.0
	for _, p := range t.parts {
		if p.charset != "" {
			bits += float64(p.length) * math.Log2(float64(len(charsets[p.charset])))
		}
	}
	return bits
}

// Literals is the template's literal text, in order.
func (t *Template) Literals() []string {
	var out []string
	for _, p := range t.parts {
		if p.charset == "" {
			out = append(out, p.literal)
		}
	}
	return out
}

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

// MinChosenBits is the least randomness a template a person wrote may mint
// with. Sentinels are how the proxy tells one credential's grants from
// another's, so two must never come out the same.
const MinChosenBits = 64

// ParseChosen compiles a template a person wrote, held to what an inferred
// one never needs holding to: braces that pair and do not nest, no literal
// brace, at least MinChosenBits of randomness, and MaxLength, since every
// sentinel minted for the secret generates it and nobody means a template
// that generates megabytes. Its refusals say where, for a person to fix.
func ParseChosen(format string) (*Template, error) {
	depth, open := 0, 0
	for i, r := range format {
		switch r {
		case '{':
			if depth > 0 {
				return nil, fmt.Errorf("the { at %d is inside another {…}; tokens don't nest", i+1)
			}
			depth, open = 1, i
		case '}':
			if depth == 0 {
				return nil, fmt.Errorf("the } at %d closes nothing; a format has no literal braces", i+1)
			}
			depth = 0
			token := format[open+1 : i]
			name, length, ok := strings.Cut(token, ":")
			if !ok {
				return nil, fmt.Errorf("{%s} needs a charset and a length, as {alnum:36}", token)
			}
			if _, known := charsets[name]; !known {
				return nil, fmt.Errorf("{%s}: unknown charset %q; it is one of %s", token, name, strings.Join(charsetOrder, ", "))
			}
			if n, err := strconv.Atoi(length); err != nil || n <= 0 {
				return nil, fmt.Errorf("{%s}: the length is a whole number above 0", token)
			}
		}
	}
	if depth > 0 {
		return nil, fmt.Errorf("the { at %d is never closed", open+1)
	}
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
	switch bits := t.RandomBits(); {
	case bits == 0:
		return nil, fmt.Errorf("it has no {charset:length} token, so every sentinel would be the same")
	case bits < MinChosenBits:
		return nil, fmt.Errorf("its {…} tokens give %.0f random bits, and a sentinel needs at least %d so no two come out the same", bits, MinChosenBits)
	}
	return t, nil
}

// CheckValue holds a template a person wrote against the value it is for.
// A format is stored in the clear, so it may copy nothing of the value but
// the prefix inference itself would keep (ghp_, github_pat_): err says what
// it copied. warning says the value does not have the template's shape,
// which a person may mean, but which a tool checking a token's shape may
// treat differently.
func CheckValue(t *Template, value string) (warning string, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	prefix := ""
	if inferred, err := Parse(Describe(value)); err == nil {
		if lits := inferred.Literals(); len(lits) > 0 && strings.HasPrefix(value, lits[0]) {
			prefix = lits[0]
		}
	}
	for _, lit := range t.Literals() {
		at := strings.Index(value, lit)
		if len(lit) >= 8 && at >= 0 && (at != 0 || len(lit) > len(prefix)) {
			return "", fmt.Errorf("the text %q is copied from the value; a format is stored in the clear, so only the value's prefix (%q) may come from it", lit, prefix)
		}
	}
	if !t.Validate(value) {
		return "The value itself doesn't have this shape. A tool that checks a token's shape may treat the two differently.", nil
	}
	return "", nil
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
// unparseable. Every sentinel in the system — each one bound to a sandbox env
// var — comes from here, so none is shaped by a rule another does not share.
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
