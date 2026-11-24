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
// object (ParseMessage): what a route's message places are matched in. It
// keeps only what is matched, recorded while the message is read: no tree of
// its values is built, so reading costs what it keeps, not a node per value.
type Message struct {
	raw []byte
	// object is whether the message is a JSON object, not another value.
	object bool
	// top is each top-level member's canonical scalar (canonicalScalar), for
	// conditions; a member whose value is an object or array is absent.
	top map[string]string
	// strings are every member name and string value, in document order.
	strings []MessageString
	// pointers are the steps of every string's JSON pointer, each naming the
	// step it is under, so a string keeps one step and not a copy of its
	// whole pointer: a long member name over many strings is kept once.
	pointers []pointerStep
}

// MessageString is one string in a message: a string value, or a member
// name. Start and End are its bytes in the message, quotes included.
type MessageString struct {
	Value      string
	Name       bool
	Start, End int
	message    *Message
	// at is the string's last step in message.pointers, -1 at the root.
	at int32
}

// pointerStep is one step of a JSON pointer under the step at parent (-1
// for the root): a member, at its name's index in Message.strings, or an
// element, at its index in its array.
type pointerStep struct {
	parent, at int32
	member     bool
}

// At is the JSON pointer of the string's value, or of the member a name
// names. It is built when asked, not kept: a caller asks it of the few
// strings it matches.
func (s MessageString) At() string {
	if s.message == nil {
		return ""
	}
	var steps []int32
	for at := s.at; at >= 0; at = s.message.pointers[at].parent {
		steps = append(steps, at)
	}
	var b []byte
	for i := len(steps) - 1; i >= 0; i-- {
		step := s.message.pointers[steps[i]]
		b = append(b, '/')
		if step.member {
			b = appendPointerSegment(b, s.message.strings[step.at].Value)
		} else {
			b = strconv.AppendInt(b, int64(step.at), 10)
		}
	}
	return string(b)
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
	if !m.object {
		return nil, ErrNotMessage
	}
	return m, nil
}

// ParseDocument reads data as one JSON value of any kind, as strictly as
// ParseMessage: what a typed route's ws= array is checked with.
//
// It reads data twice. The first read refuses what is no message and counts
// the strings and pointer steps there are; the second records them in slices
// of that size. So a refused document records nothing, and an accepted one
// allocates what it keeps, never room for strings it turns out not to hold.
func ParseDocument(data []byte) (*Message, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%w: invalid UTF-8", ErrNotMessage)
	}
	count := &jsonParser{data: data}
	kind, err := count.document()
	if err != nil {
		return nil, err
	}
	m := &Message{
		raw:      data,
		object:   kind == kindObject,
		top:      map[string]string{},
		strings:  make([]MessageString, 0, count.strings),
		pointers: make([]pointerStep, 0, count.steps),
	}
	if _, err := (&jsonParser{data: data, record: m, decoded: count.decoded}).document(); err != nil {
		return nil, err
	}
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

// EscapePointer is a member name as one segment of a JSON pointer (RFC 6901):
// "~" is "~0" and "/" is "~1".
func EscapePointer(name string) string {
	return string(appendPointerSegment(nil, name))
}

// appendPointerSegment appends name to b as one JSON pointer segment
// (EscapePointer).
func appendPointerSegment(b []byte, name string) []byte {
	for i := 0; i < len(name); i++ {
		switch c := name[i]; c {
		case '~':
			b = append(b, '~', '0')
		case '/':
			b = append(b, '~', '1')
		default:
			b = append(b, c)
		}
	}
	return b
}

type jsonKind uint8

const (
	kindObject jsonKind = iota + 1
	kindArray
	kindString
	kindNumber
	kindLiteral // true, false, null
)

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

// jsonParser reads one value strictly. It builds no tree: when record is set
// it appends each string, with where it is, and each canonical scalar of the
// root object's members, to record as it reads them; either way it counts
// the strings and pointer steps it would record.
type jsonParser struct {
	data   []byte
	pos    int
	record *Message
	// path is where the value being read is, one step per container it is
	// in. A step's node is its index among the pointer steps once a string
	// under it has been kept, -1 until then, so a container holding no string
	// keeps no step.
	path []pathStep
	// strings and steps count the strings and pointer steps kept so far.
	strings, steps int32
	// decoded is the string being read, decoded: one buffer for them all.
	decoded []byte
}

// pathStep is one step of path: a member, at its name's index among the
// strings, or an element, at its index (pointerStep).
type pathStep struct {
	at, node int32
	member   bool
}

// document reads data as one value and nothing after it but whitespace.
func (p *jsonParser) document() (jsonKind, error) {
	kind, _, err := p.value(0, false)
	if err != nil {
		return 0, err
	}
	p.space()
	if p.pos != len(p.data) {
		return 0, fmt.Errorf("%w: more than one value", ErrNotMessage)
	}
	return kind, nil
}

// keep records s, a string read where path says, when there is a record.
func (p *jsonParser) keep(s MessageString) {
	at := p.pointer()
	p.strings++
	if p.record != nil {
		s.message, s.at = p.record, at
		p.record.strings = append(p.record.strings, s)
	}
}

// pointer keeps the steps of path not yet kept and returns the last one's
// index, -1 at the root. A step kept once is shared by every string under it.
func (p *jsonParser) pointer() int32 {
	i := len(p.path)
	for i > 0 && p.path[i-1].node < 0 {
		i--
	}
	parent := int32(-1)
	if i > 0 {
		parent = p.path[i-1].node
	}
	for ; i < len(p.path); i++ {
		step := &p.path[i]
		if p.record != nil {
			p.record.pointers = append(p.record.pointers, pointerStep{parent: parent, at: step.at, member: step.member})
		}
		step.node = p.steps
		p.steps++
		parent = step.node
	}
	return parent
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

// value reads one value at depth containers deep and returns its kind. When
// canonical is set and the value is a scalar, scalar is its canonical
// spelling (canonicalScalar): a string JSON-encoded from its decoded value, a
// number in its canonical decimal, a literal as it is.
func (p *jsonParser) value(depth int, canonical bool) (kind jsonKind, scalar string, err error) {
	p.space()
	if p.pos >= len(p.data) {
		return 0, "", p.fail("unexpected end")
	}
	start := p.pos
	switch c := p.data[p.pos]; {
	case c == '{' || c == '[':
		if depth >= MaxMessageDepth {
			return 0, "", p.fail("nested too deeply")
		}
		if c == '{' {
			return kindObject, "", p.object(depth + 1)
		}
		return kindArray, "", p.array(depth + 1)
	case c == '"':
		text, err := p.str(p.record != nil || canonical)
		if err != nil {
			return 0, "", err
		}
		p.keep(MessageString{Value: text, Start: start, End: p.pos})
		if canonical {
			scalar = encodeJSONString(text)
		}
		return kindString, scalar, nil
	case c == '-' || c >= '0' && c <= '9':
		scalar, err := p.number(canonical)
		if err != nil {
			return 0, "", err
		}
		return kindNumber, scalar, nil
	default:
		for _, literal := range []string{"true", "false", "null"} {
			if bytes.HasPrefix(p.data[p.pos:], []byte(literal)) {
				p.pos += len(literal)
				return kindLiteral, literal, nil
			}
		}
		return 0, "", p.fail("invalid value")
	}
}

// object reads an object at depth containers deep; at depth 1 it is the root,
// whose members' scalars are recorded for conditions.
func (p *jsonParser) object(depth int) error {
	p.pos++
	seen := map[string]struct{}{}
	p.space()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		return nil
	}
	p.path = append(p.path, pathStep{member: true, node: -1})
	defer func() { p.path = p.path[:len(p.path)-1] }()
	top := depth == 1 && p.record != nil
	for {
		p.space()
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return p.fail("expected a member name")
		}
		nameStart := p.pos
		name, err := p.str(true)
		if err != nil {
			return err
		}
		if _, twice := seen[name]; twice {
			return p.fail("a member named twice")
		}
		seen[name] = struct{}{}
		p.path[len(p.path)-1] = pathStep{member: true, at: p.strings, node: -1}
		p.keep(MessageString{Value: name, Name: true, Start: nameStart, End: p.pos})
		p.space()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return p.fail("expected :")
		}
		p.pos++
		kind, scalar, err := p.value(depth, top)
		if err != nil {
			return err
		}
		if top && kind != kindObject && kind != kindArray {
			p.record.top[name] = scalar
		}
		p.space()
		if p.pos >= len(p.data) {
			return p.fail("unexpected end")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return nil
		default:
			return p.fail("expected , or }")
		}
	}
}

func (p *jsonParser) array(depth int) error {
	p.pos++
	p.space()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		return nil
	}
	p.path = append(p.path, pathStep{node: -1})
	defer func() { p.path = p.path[:len(p.path)-1] }()
	for i := 0; ; i++ {
		p.path[len(p.path)-1] = pathStep{at: int32(i), node: -1}
		if _, _, err := p.value(depth, false); err != nil {
			return err
		}
		p.space()
		if p.pos >= len(p.data) {
			return p.fail("unexpected end")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return nil
		default:
			return p.fail("expected , or ]")
		}
	}
}

// str reads a string, decoding its escapes into p.decoded, and returns it
// when keep is set: one read only to refuse or count allocates nothing for
// it. A raw control character, an unknown escape, and a surrogate escape not
// part of a pair are refused.
func (p *jsonParser) str(keep bool) (string, error) {
	p.pos++ // the opening quote
	// Decoded, a string is no longer than it is written, so one buffer that
	// long holds it without growing.
	end := p.pos
	for end < len(p.data) && p.data[end] != '"' {
		if p.data[end] == '\\' {
			end++
		}
		end++
	}
	if cap(p.decoded) < end-p.pos {
		p.decoded = make([]byte, 0, end-p.pos)
	}
	b := p.decoded[:0]
	defer func() { p.decoded = b }()
	for {
		if p.pos >= len(p.data) {
			return "", p.fail("unterminated string")
		}
		c := p.data[p.pos]
		switch {
		case c == '"':
			p.pos++
			if !keep {
				return "", nil
			}
			return string(b), nil
		case c < 0x20:
			return "", p.fail("control character in a string")
		case c != '\\':
			b = append(b, c)
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
			b = append(b, e)
		case 'b':
			b = append(b, '\b')
		case 'f':
			b = append(b, '\f')
		case 'n':
			b = append(b, '\n')
		case 'r':
			b = append(b, '\r')
		case 't':
			b = append(b, '\t')
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
			b = utf8.AppendRune(b, r)
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

// number reads a number by JSON's grammar and, when canonical is set, returns
// its canonical decimal (canonicalNumber). A number whose exponent is beyond
// maxExponent either way is refused whether or not it is kept.
func (p *jsonParser) number(canonical bool) (string, error) {
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
	intFrom := digitsFrom()
	integer := p.data[intFrom:p.pos]
	if len(integer) == 0 || len(integer) > 1 && integer[0] == '0' {
		return "", p.fail("invalid number")
	}
	var fraction []byte
	if p.pos < len(p.data) && p.data[p.pos] == '.' {
		p.pos++
		from := digitsFrom()
		fraction = p.data[from:p.pos]
		if len(fraction) == 0 {
			return "", p.fail("invalid number")
		}
	}
	var exponent int64
	if p.pos < len(p.data) && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		p.pos++
		negativeExponent := false
		if p.pos < len(p.data) && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			negativeExponent = p.data[p.pos] == '-'
			p.pos++
		}
		from := digitsFrom()
		if from == p.pos {
			return "", p.fail("invalid number")
		}
		for _, c := range p.data[from:p.pos] {
			if exponent = exponent*10 + int64(c-'0'); exponent > maxExponent {
				p.pos = start
				return "", p.fail("number out of range")
			}
		}
		if negativeExponent {
			exponent = -exponent
		}
	}
	if !canonical {
		return "", nil
	}
	return canonicalNumber(negative, string(integer)+string(fraction), len(fraction), exponent), nil
}

// maxExponent bounds a number's exponent, so its canonical form is small.
const maxExponent = 1 << 20

// canonicalNumber is the one spelling of an exact decimal value: digits
// (integer and fraction digits, run together) times ten to exp less the
// fraction's length, exp within maxExponent (number refuses one beyond it). Leading and trailing zeros are dropped into the
// exponent, so equal values have equal digits and exponent, and the spelling
// chosen from those two is unique: plain for a modest exponent, scientific
// otherwise. Zero is "0" whatever its sign.
func canonicalNumber(negative bool, digits string, fractionLen int, exp int64) string {
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		return "0"
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
	return b.String()
}

// canonicalScalar reads raw as one JSON scalar, strictly, and returns its
// canonical spelling: a string, a number, true, false or null. An object or
// array is refused.
func canonicalScalar(raw []byte) (string, error) {
	if !utf8.Valid(raw) {
		return "", fmt.Errorf("invalid UTF-8")
	}
	p := &jsonParser{data: raw}
	kind, scalar, err := p.value(0, true)
	if err != nil {
		return "", err
	}
	p.space()
	if p.pos != len(raw) {
		return "", fmt.Errorf("more than one value")
	}
	if kind == kindObject || kind == kindArray {
		return "", fmt.Errorf("an object or array is no condition: a condition is a string, number, true, false or null")
	}
	return scalar, nil
}
