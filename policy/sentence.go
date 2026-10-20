package policy

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Grammar is a statement's sentence form, shown with every SyntaxError so an
// agent that wrote a malformed sentence can write it again.
const Grammar = `allow|deny <action>[, <action>…] on <type> [<name>] [created by $this] [within <type> [<name>] [created by $this]…] in <type> [<name>] [of <type> [<name>] [created by $this]…] [when "<condition>"] [unless "<condition>"]; ` +
	`a type is unquoted words, a name is "quoted" or a variable, within and of read from the inside out, and a deny may say in anywhere`

// keywords end an action or a type, so neither may hold one as a word.
var keywords = []string{"on", "within", "in", "of", "when", "unless", "created"}

// SyntaxError is a sentence that is not a statement: what was found where,
// and what was expected there.
type SyntaxError struct {
	Sentence string
	// Offset is the byte offset of Found in Sentence.
	Offset   int
	Found    string
	Expected string
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("at %s (byte %d): expected %s; a statement reads: %s", e.Found, e.Offset, e.Expected, Grammar)
}

// Parse reads one statement from its sentence. It produces structure and does
// not decide whether the statement is valid: that is Policy.Validate's.
func Parse(sentence string) (Statement, error) {
	p := &parser{sentence: sentence}
	s, err := p.statement()
	if err != nil {
		return Statement{}, err
	}
	return s, nil
}

// ParseStatements reads each sentence as a statement, in order. It reports
// every sentence that does not parse, joined.
func ParseStatements(sentences []string) ([]Statement, error) {
	statements := make([]Statement, 0, len(sentences))
	var errs []error
	for i, sentence := range sentences {
		s, err := Parse(sentence)
		if err != nil {
			errs = append(errs, fmt.Errorf("statement %d: %w", i+1, err))
			continue
		}
		statements = append(statements, s)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return statements, nil
}

// Format writes s as its canonical sentence. Parse reads it back to s for
// every statement Validate accepts. Quoted text is written in ASCII, every
// other rune escaped: outside ASCII a letter, a mark or a blank can pass for a
// closing quote, and no rule short of ASCII was found that shows only runes
// that cannot.
func Format(s Statement) string {
	var b strings.Builder
	b.WriteString(string(s.Effect))
	b.WriteString(" ")
	b.WriteString(strings.Join(s.Actions, ", "))
	b.WriteString(" on ")
	writeResource(&b, Resource{Type: s.ResourceType, Name: s.Name, CreatedBy: s.CreatedBy})
	for _, parent := range slices.Backward(s.Within) {
		b.WriteString(" within ")
		writeResource(&b, parent)
	}
	if s.Scope.Type != "" {
		b.WriteString(" in ")
		b.WriteString(s.Scope.Type)
		if s.Scope.Name != "" {
			b.WriteString(" ")
			b.WriteString(formatName(s.Scope.Name))
		}
		for _, parent := range slices.Backward(s.Scope.Within) {
			b.WriteString(" of ")
			writeResource(&b, parent)
		}
	}
	if s.When != nil {
		b.WriteString(" when ")
		b.WriteString(strconv.QuoteToASCII(*s.When))
	}
	if s.Unless != nil {
		b.WriteString(" unless ")
		b.WriteString(strconv.QuoteToASCII(*s.Unless))
	}
	return b.String()
}

// FormatStatements writes each statement as its canonical sentence.
func FormatStatements(statements []Statement) []string {
	sentences := make([]string, len(statements))
	for i, s := range statements {
		sentences[i] = Format(s)
	}
	return sentences
}

func writeResource(b *strings.Builder, r Resource) {
	b.WriteString(r.Type)
	if r.Name != nil {
		b.WriteString(" ")
		b.WriteString(formatName(*r.Name))
	}
	if r.CreatedBy != "" {
		b.WriteString(" created by ")
		b.WriteString(formatName(r.CreatedBy))
	}
}

// formatName writes a variable a policy may use bare and anything else
// quoted, so only a real variable reads as one.
func formatName(name string) string {
	if IsVariable(name) {
		return name
	}
	return strconv.QuoteToASCII(name)
}

// checkSentenceWord reports whether w, an action or a type, can be written
// in a sentence and read back as itself, and reads as what it is: printable
// ASCII words, one space apart, none of them a keyword. Format writes a word
// as it is, so a rune outside ASCII could look like a space, a quote or a
// variable and let a sentence read narrower than its statement; there is no
// list of such runes to refuse, only a set of safe ones to allow.
func checkSentenceWord(w string) error {
	for i := 0; i < len(w); i++ {
		switch c := w[i]; {
		case c < ' ' || c > '~':
			return fmt.Errorf("%q holds a byte outside printable ASCII, which a word may not", w)
		case c == '"':
			return fmt.Errorf("%q has a quote; a name goes in quotes after its type", w)
		case c == ',':
			return fmt.Errorf("%q has a comma, which a sentence reads as the end of an action", w)
		case c == '$':
			return fmt.Errorf("%q has a $, which a sentence reads as a variable", w)
		}
	}
	for part := range strings.SplitSeq(w, " ") {
		switch {
		case part == "":
			return fmt.Errorf("%q has two spaces in a row", w)
		case slices.Contains(keywords, part):
			return fmt.Errorf("%q has the word %q, which a sentence reads as a keyword", w, part)
		}
	}
	return nil
}

type tokenKind int

const (
	tokenEnd tokenKind = iota
	tokenWord
	tokenVariable
	tokenQuoted
	tokenComma
)

type token struct {
	kind   tokenKind
	text   string // the value: a word, a variable, or a quoted string unquoted
	raw    string // as written
	offset int
}

// describe names t for an error. What the sentence holds is escaped: an
// error is shown, and the sentence came from inside a discobox.
func (t token) describe() string {
	switch t.kind {
	case tokenEnd:
		return "the end of the sentence"
	case tokenQuoted:
		return "the name or condition " + escape(t.raw)
	case tokenVariable:
		return "the variable " + escape(t.raw)
	case tokenComma:
		return `","`
	}
	return strconv.Quote(t.raw)
}

// escape writes each rune of text that is not printable as its Go escape.
func escape(text string) string {
	var b strings.Builder
	for _, r := range text {
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		} else {
			q := strconv.QuoteRune(r)
			b.WriteString(q[1 : len(q)-1])
		}
	}
	return b.String()
}

func (t token) is(keyword string) bool { return t.kind == tokenWord && t.text == keyword }

// parser reads a sentence token by token, left to right, with one token of
// lookahead.
type parser struct {
	sentence string
	pos      int
	peeked   *token
	err      error
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func (p *parser) peek() token {
	if p.peeked != nil {
		return *p.peeked
	}
	t := p.lex()
	p.peeked = &t
	return t
}

func (p *parser) next() token {
	t := p.peek()
	p.peeked = nil
	return t
}

func (p *parser) lex() token {
	s := p.sentence
	for p.pos < len(s) && isSpace(s[p.pos]) {
		p.pos++
	}
	start := p.pos
	if start == len(s) {
		return token{kind: tokenEnd, offset: start}
	}
	switch s[start] {
	case ',':
		p.pos++
		return token{kind: tokenComma, text: ",", raw: ",", offset: start}
	case '"':
		end := start + 1
		for end < len(s) && s[end] != '"' {
			if s[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(s) {
			p.pos = len(s)
			p.fail(token{kind: tokenEnd, offset: len(s)}, `a closing " for the quote opened at byte `+strconv.Itoa(start))
			return token{kind: tokenEnd, offset: len(s)}
		}
		p.pos = end + 1
		raw := s[start:p.pos]
		text, err := strconv.Unquote(raw)
		t := token{kind: tokenQuoted, text: text, raw: raw, offset: start}
		if err != nil {
			p.fail(t, `a quoted string whose backslashes are escapes Go's strconv reads (\" \\ \n é)`)
		}
		return t
	}
	for p.pos < len(s) && !isSpace(s[p.pos]) && s[p.pos] != ',' && s[p.pos] != '"' {
		p.pos++
	}
	raw := s[start:p.pos]
	kind := tokenWord
	if strings.HasPrefix(raw, "$") {
		kind = tokenVariable
	}
	return token{kind: kind, text: raw, raw: raw, offset: start}
}

// fail records the first error; the parse stops at it.
func (p *parser) fail(found token, expected string) {
	if p.err == nil {
		p.err = &SyntaxError{Sentence: p.sentence, Offset: found.offset, Found: found.describe(), Expected: expected}
	}
}

func (p *parser) statement() (Statement, error) {
	var s Statement
	switch t := p.next(); {
	case t.is(string(Allow)), t.is(string(Deny)):
		s.Effect = Effect(t.text)
	default:
		p.fail(t, `"allow" or "deny" to start the statement`)
		return s, p.err
	}

	for {
		action := p.words(`an action, such as read or push`)
		if p.err != nil {
			return s, p.err
		}
		s.Actions = append(s.Actions, action)
		t := p.next()
		if t.kind == tokenComma {
			continue
		}
		if !t.is("on") {
			p.fail(t, `"," and another action, or "on" and the resource type`)
			return s, p.err
		}
		break
	}

	r := p.resource("the resource type, such as branch or pull request")
	s.ResourceType, s.Name, s.CreatedBy = r.Type, r.Name, r.CreatedBy
	for p.err == nil && p.peek().is("within") {
		p.next()
		s.Within = append(s.Within, p.resource("the type of the resource it is within, such as issue"))
	}
	slices.Reverse(s.Within)

	if p.err == nil && p.peek().is("in") {
		p.next()
		s.Scope.Type = p.words(`the scope's type, such as repository, or anywhere`)
		if t := p.peek(); p.err == nil && (t.kind == tokenQuoted || t.kind == tokenVariable) {
			s.Scope.Name = p.next().text
		}
		for p.err == nil && p.peek().is("of") {
			p.next()
			s.Scope.Within = append(s.Scope.Within, p.resource("the type of the scope it is of, such as cluster"))
		}
		slices.Reverse(s.Scope.Within)
	}

	for p.err == nil {
		t := p.next()
		var condition **string
		switch {
		case t.kind == tokenEnd:
			return s, p.err
		case t.is("when") && s.When == nil:
			condition = &s.When
		case t.is("unless") && s.Unless == nil:
			condition = &s.Unless
		default:
			p.fail(t, p.expectedAfter(s))
			return s, p.err
		}
		c := p.next()
		if c.kind != tokenQuoted {
			p.fail(c, fmt.Sprintf(`the condition after %q, in quotes`, t.text))
			return s, p.err
		}
		*condition = &c.text
	}
	return s, p.err
}

// expectedAfter says what may follow the part of s already read.
func (p *parser) expectedAfter(s Statement) string {
	var next []string
	switch noCondition := s.When == nil && s.Unless == nil; {
	case noCondition && s.Scope.Type == "":
		next = append(next, `"within" and a parent`, `"in" and the scope`)
	case noCondition:
		next = append(next, `"of" and what the scope is in`)
	}
	if s.When == nil {
		next = append(next, `"when" and a condition`)
	}
	if s.Unless == nil {
		next = append(next, `"unless" and a condition`)
	}
	return strings.Join(next, ", ") + ", or the end of the sentence"
}

// resource reads a type, an optional name, and an optional "created by".
func (p *parser) resource(what string) Resource {
	r := Resource{Type: p.words(what)}
	if t := p.peek(); p.err == nil && (t.kind == tokenQuoted || t.kind == tokenVariable) {
		name := p.next().text
		r.Name = &name
	}
	if p.err == nil && p.peek().is("created") {
		p.next()
		if t := p.next(); !t.is("by") {
			p.fail(t, `"by" after "created"`)
			return r
		}
		t := p.next()
		if t.kind != tokenQuoted && t.kind != tokenVariable {
			p.fail(t, `$this after "created by"`)
			return r
		}
		r.CreatedBy = t.text
	}
	return r
}

// words reads one or more unquoted words that are not keywords, joined by a
// space: an action or a type.
func (p *parser) words(what string) string {
	var words []string
	for p.err == nil {
		t := p.peek()
		if t.kind != tokenWord || slices.Contains(keywords, t.text) {
			break
		}
		words = append(words, p.next().text)
	}
	if p.err == nil && len(words) == 0 {
		p.fail(p.peek(), what)
	}
	return strings.Join(words, " ")
}
