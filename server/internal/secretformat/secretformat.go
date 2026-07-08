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
	re    *regexp.Regexp
}

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
	t := &Template{raw: format, parts: parts}
	re, err := compileRegex(parts)
	if err != nil {
		return nil, err
	}
	t.re = re
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
	return t.re != nil && t.re.MatchString(value)
}

func compileRegex(parts []part) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for _, p := range parts {
		if p.charset == "" {
			b.WriteString(regexp.QuoteMeta(p.literal))
			continue
		}
		b.WriteString("[")
		b.WriteString(charClass(charsets[p.charset]))
		b.WriteString("]{")
		b.WriteString(strconv.Itoa(p.length))
		b.WriteString("}")
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// charClass escapes a charset alphabet for use inside a regex character class.
func charClass(alphabet string) string {
	var b strings.Builder
	for _, r := range alphabet {
		switch r {
		case '-', ']', '^', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
