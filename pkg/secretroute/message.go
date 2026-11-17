package secretroute

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// A WebSocket message a credential may be swapped into is read strictly
// (ADR 26-11-06-442 §1): exactly one JSON object with only JSON whitespace
// around it, valid UTF-8, no unpaired surrogate escape, no object naming a
// member twice at any depth (after unescaping), and at most
// MaxMessageDepth containers deep. Anything else is no message a place can
// be read in. Numbers are kept as exact decimals, never floats, so a
// condition compares mathematical values: 2 and 2.0 are equal, and two
// numbers a float would round together are not.

// MaxMessageDepth is how deeply nested a message may be, in objects and
// arrays.
const MaxMessageDepth = 64

// Message is one client-to-server WebSocket text message read as a JSON
// object (ParseMessage): what a route's message places are matched in.
type Message struct {
	raw  []byte
	root *jsonNode
	// top is each top-level member's canonical scalar (canonicalScalar), for
	// conditions; a member whose value is an object or array is absent.
	top map[string]string
	// strings are every member name and string value, in document order.
	strings []MessageString
}

// MessageString is one string in a message: a string value, or a member
// name. At is the JSON pointer of the value, or of the member a name names.
// Start and End are its bytes in the message, quotes included.
type MessageString struct {
	At         string
	Value      string
	Name       bool
	Start, End int
}

// ErrNotMessage is a message that is not exactly one JSON object a place can
// be read in.
var ErrNotMessage = errors.New("not one JSON object with each member named once")

// ParseMessage reads data as one JSON object, strictly.
func ParseMessage(data []byte) (*Message, error) {
	m, err := ParseDocument(data)
	if err != nil {
		return nil, err
	}
	if m.root.kind != kindObject {
		return nil, ErrNotMessage
	}
	return m, nil
}

// ParseDocument reads data as one JSON value of any kind, as strictly as
// ParseMessage: what a JSON request body's places are read in, so a body and
// a message resolve pointers, escapes and duplicate members alike.
func ParseDocument(data []byte) (*Message, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%w: invalid UTF-8", ErrNotMessage)
	}
	p := &jsonParser{data: data}
	root, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.space()
	if p.pos != len(data) {
		return nil, fmt.Errorf("%w: more than one value", ErrNotMessage)
	}
	m := &Message{raw: data, root: root, top: map[string]string{}}
	for _, member := range root.members {
		if scalar, ok := member.value.canonical(); ok {
			m.top[member.name] = scalar
		}
	}
	m.collect(root, "")
	return m, nil
}

// Raw is the message as it was read.
func (m *Message) Raw() []byte { return m.raw }

// Strings are every member name and string value in the message.
func (m *Message) Strings() []MessageString { return m.strings }

// holds reports whether the message's top-level member name is the scalar a
// condition names, both canonical (canonicalScalar). A missing member is not
// null.
func (m *Message) holds(name, scalar string) bool {
	value, ok := m.top[name]
	return ok && value == scalar
}

func (m *Message) collect(node *jsonNode, at string) {
	switch node.kind {
	case kindObject:
		for _, member := range node.members {
			child := at + "/" + EscapePointer(member.name)
			m.strings = append(m.strings, MessageString{At: child, Value: member.name, Name: true, Start: member.start, End: member.end})
			m.collect(member.value, child)
		}
	case kindArray:
		for i, elem := range node.elems {
			m.collect(elem, at+"/"+strconv.Itoa(i))
		}
	case kindString:
		m.strings = append(m.strings, MessageString{At: at, Value: node.text, Start: node.start, End: node.end})
	}
}

// EscapePointer is a member name as one segment of a JSON pointer (RFC 6901):
// "~" is "~0" and "/" is "~1".
func EscapePointer(name string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(name)
}

type jsonKind uint8

const (
	kindObject jsonKind = iota + 1
	kindArray
	kindString
	kindNumber
	kindLiteral // true, false, null
)

// jsonNode is one value read strictly. text is a string's decoded value, a
// number's canonical decimal, or a literal as written.
type jsonNode struct {
	kind       jsonKind
	text       string
	start, end int
	members    []jsonMember
	elems      []*jsonNode
}

type jsonMember struct {
	name       string
	start, end int
	value      *jsonNode
}

// canonical is a scalar node as a condition compares it: a string JSON-encoded
// from its decoded value, a number in its canonical decimal, a literal as it
// is. Objects and arrays are no scalar.
func (n *jsonNode) canonical() (string, bool) {
	switch n.kind {
	case kindString:
		return encodeJSONString(n.text), true
	case kindNumber, kindLiteral:
		return n.text, true
	}
	return "", false
}

// encodeJSONString is s as a JSON string, the one spelling every equal string
// has: HTML characters are not escaped.
func encodeJSONString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		// A string always encodes; one that did not would be no condition.
		panic(fmt.Sprintf("secretroute: encode a JSON string: %v", err))
	}
	return strings.TrimSuffix(b.String(), "\n")
}

type jsonParser struct {
	data []byte
	pos  int
}

func (p *jsonParser) space() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *jsonParser) fail(what string) error {
	return fmt.Errorf("%w: %s at byte %d", ErrNotMessage, what, p.pos)
}

// value reads one value at depth containers deep.
func (p *jsonParser) value(depth int) (*jsonNode, error) {
	p.space()
	if p.pos >= len(p.data) {
		return nil, p.fail("unexpected end")
	}
	start := p.pos
	switch c := p.data[p.pos]; {
	case c == '{' || c == '[':
		if depth >= MaxMessageDepth {
			return nil, p.fail("nested too deeply")
		}
		if c == '{' {
			return p.object(depth + 1)
		}
		return p.array(depth + 1)
	case c == '"':
		text, err := p.str()
		if err != nil {
			return nil, err
		}
		return &jsonNode{kind: kindString, text: text, start: start, end: p.pos}, nil
	case c == '-' || c >= '0' && c <= '9':
		text, err := p.number()
		if err != nil {
			return nil, err
		}
		return &jsonNode{kind: kindNumber, text: text, start: start, end: p.pos}, nil
	default:
		for _, literal := range []string{"true", "false", "null"} {
			if bytes.HasPrefix(p.data[p.pos:], []byte(literal)) {
				p.pos += len(literal)
				return &jsonNode{kind: kindLiteral, text: literal, start: start, end: p.pos}, nil
			}
		}
		return nil, p.fail("invalid value")
	}
}

func (p *jsonParser) object(depth int) (*jsonNode, error) {
	node := &jsonNode{kind: kindObject, start: p.pos}
	p.pos++
	seen := map[string]struct{}{}
	p.space()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		node.end = p.pos
		return node, nil
	}
	for {
		p.space()
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return nil, p.fail("expected a member name")
		}
		nameStart := p.pos
		name, err := p.str()
		if err != nil {
			return nil, err
		}
		if _, twice := seen[name]; twice {
			return nil, p.fail("a member named twice")
		}
		seen[name] = struct{}{}
		member := jsonMember{name: name, start: nameStart, end: p.pos}
		p.space()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return nil, p.fail("expected :")
		}
		p.pos++
		if member.value, err = p.value(depth); err != nil {
			return nil, err
		}
		node.members = append(node.members, member)
		p.space()
		if p.pos >= len(p.data) {
			return nil, p.fail("unexpected end")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			node.end = p.pos
			return node, nil
		default:
			return nil, p.fail("expected , or }")
		}
	}
}

func (p *jsonParser) array(depth int) (*jsonNode, error) {
	node := &jsonNode{kind: kindArray, start: p.pos}
	p.pos++
	p.space()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		node.end = p.pos
		return node, nil
	}
	for {
		elem, err := p.value(depth)
		if err != nil {
			return nil, err
		}
		node.elems = append(node.elems, elem)
		p.space()
		if p.pos >= len(p.data) {
			return nil, p.fail("unexpected end")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			node.end = p.pos
			return node, nil
		default:
			return nil, p.fail("expected , or ]")
		}
	}
}

// str reads a string, decoding its escapes. A raw control character, an
// unknown escape, and a surrogate escape not part of a pair are refused.
func (p *jsonParser) str() (string, error) {
	p.pos++ // the opening quote
	var b strings.Builder
	for {
		if p.pos >= len(p.data) {
			return "", p.fail("unterminated string")
		}
		c := p.data[p.pos]
		switch {
		case c == '"':
			p.pos++
			return b.String(), nil
		case c < 0x20:
			return "", p.fail("control character in a string")
		case c != '\\':
			b.WriteByte(c)
			p.pos++
			continue
		}
		if p.pos+1 >= len(p.data) {
			return "", p.fail("unterminated escape")
		}
		e := p.data[p.pos+1]
		p.pos += 2
		switch e {
		case '"', '\\', '/':
			b.WriteByte(e)
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			r, ok := p.hex4()
			if !ok {
				return "", p.fail("invalid \\u escape")
			}
			switch {
			case utf16.IsSurrogate(r) && r < 0xdc00:
				if p.pos+1 >= len(p.data) || p.data[p.pos] != '\\' || p.data[p.pos+1] != 'u' {
					return "", p.fail("unpaired surrogate escape")
				}
				p.pos += 2
				low, ok := p.hex4()
				if !ok || low < 0xdc00 || low > 0xdfff {
					return "", p.fail("unpaired surrogate escape")
				}
				r = utf16.DecodeRune(r, low)
			case utf16.IsSurrogate(r):
				return "", p.fail("unpaired surrogate escape")
			}
			b.WriteRune(r)
		default:
			return "", p.fail("invalid escape")
		}
	}
}

func (p *jsonParser) hex4() (rune, bool) {
	if p.pos+4 > len(p.data) {
		return 0, false
	}
	v, err := strconv.ParseUint(string(p.data[p.pos:p.pos+4]), 16, 32)
	if err != nil {
		return 0, false
	}
	p.pos += 4
	return rune(v), true
}

// number reads a number by JSON's grammar and returns its canonical decimal
// (canonicalNumber).
func (p *jsonParser) number() (string, error) {
	start := p.pos
	negative := p.data[p.pos] == '-'
	if negative {
		p.pos++
	}
	digitsFrom := func() int {
		from := p.pos
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
		return from
	}
	from := digitsFrom()
	integer := string(p.data[from:p.pos])
	if integer == "" || len(integer) > 1 && integer[0] == '0' {
		return "", p.fail("invalid number")
	}
	fraction := ""
	if p.pos < len(p.data) && p.data[p.pos] == '.' {
		p.pos++
		from = digitsFrom()
		fraction = string(p.data[from:p.pos])
		if fraction == "" {
			return "", p.fail("invalid number")
		}
	}
	exponent := "0"
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		p.pos++
		sign := ""
		if p.pos < len(p.data) && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			sign = string(p.data[p.pos])
			p.pos++
		}
		from = digitsFrom()
		if from == p.pos {
			return "", p.fail("invalid number")
		}
		exponent = sign + string(p.data[from:p.pos])
	}
	text, ok := canonicalNumber(negative, integer+fraction, len(fraction), exponent)
	if !ok {
		p.pos = start
		return "", p.fail("number out of range")
	}
	return text, nil
}

// maxExponent bounds a number's exponent, so its canonical form is small.
const maxExponent = 1 << 20

// canonicalNumber is the one spelling of an exact decimal value: digits
// (integer and fraction digits, run together) times ten to the exponent less
// the fraction's length. Leading and trailing zeros are dropped into the
// exponent, so equal values have equal digits and exponent, and the spelling
// chosen from those two is unique: plain for a modest exponent, scientific
// otherwise. Zero is "0" whatever its sign.
func canonicalNumber(negative bool, digits string, fractionLen int, exponent string) (string, bool) {
	exp, err := strconv.ParseInt(strings.TrimPrefix(exponent, "+"), 10, 64)
	if err != nil || exp > maxExponent || exp < -maxExponent {
		return "", false
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0", true
	}
	e := exp - int64(fractionLen)
	trimmed := strings.TrimRight(digits, "0")
	e += int64(len(digits) - len(trimmed))
	digits = trimmed
	var b strings.Builder
	if negative {
		b.WriteByte('-')
	}
	point := int64(len(digits)) + e // where the decimal point falls in digits
	switch {
	case e >= 0 && point <= 21:
		b.WriteString(digits)
		b.WriteString(strings.Repeat("0", int(e)))
	case e < 0 && point > 0:
		b.WriteString(digits[:point])
		b.WriteByte('.')
		b.WriteString(digits[point:])
	case e < 0 && point > -6:
		b.WriteString("0.")
		b.WriteString(strings.Repeat("0", int(-point)))
		b.WriteString(digits)
	default:
		b.WriteString(digits)
		b.WriteByte('e')
		b.WriteString(strconv.FormatInt(e, 10))
	}
	return b.String(), true
}

// canonicalScalar reads raw as one JSON scalar, strictly, and returns its
// canonical spelling: a string, a number, true, false or null. An object or
// array is refused.
func canonicalScalar(raw []byte) (string, error) {
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("invalid UTF-8")
	}
	p := &jsonParser{data: raw}
	node, err := p.value(0)
	if err != nil {
		return "", err
	}
	p.space()
	if p.pos != len(raw) {
		return "", fmt.Errorf("more than one value")
	}
	scalar, ok := node.canonical()
	if !ok {
		return "", fmt.Errorf("an object or array is no condition: a condition is a string, number, true, false or null")
	}
	return scalar, nil
}
