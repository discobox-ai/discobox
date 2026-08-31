package secretformat

import (
	"regexp"
	"strconv"
	"strings"
)

// Provider describes a known credential provider: how its keys are shaped.
//
// It carries no host. Which host a credential belongs to is a binding somebody
// sets on the secret, not a fact to be guessed from four leading characters: a
// guess that is wrong is a secret bound where it does not belong, and a guess
// that is too narrow is the credential that plainly answers a request being the
// one an approver cannot pick.
type Provider struct {
	Name   string
	Prefix string // literal prefix used to recognize a value
	Format string // generative template
}

// providers is the seed table, ordered longest-prefix-first so specific
// prefixes win over generic ones (e.g. sk-ant- before sk-).
//
// It holds only the shapes structural inference cannot reconstruct. A sentinel
// has to survive whatever the harness does with it before the proxy ever sees
// it, and an SDK that checks for `sk-ant-` or `github_pat_` rejects a lookalike
// that kept only `sk-` or `github_`. Where Infer already lands on the same
// template — a plain `sk-` or `ghp_` key — there is nothing here to add, and a
// row that says what the value says is a row to keep up to date for nothing.
var providers = []Provider{
	{Name: "anthropic", Prefix: "sk-ant-", Format: "sk-ant-{alnum:5}-{base64url:95}"},
	{Name: "openai-project", Prefix: "sk-proj-", Format: "sk-proj-{base64url:48}"},
	{Name: "github-pat", Prefix: "github_pat_", Format: "github_pat_{base62:82}"},
	{Name: "slack-bot", Prefix: "xoxb-", Format: "xoxb-{digits:13}-{digits:13}-{base62:24}"},
	{Name: "aws-access-key", Prefix: "AKIA", Format: "AKIA{base32:16}"},
}

// MatchProvider returns the seed provider whose prefix matches value, if any.
func MatchProvider(value string) (Provider, bool) {
	for _, p := range providers {
		if strings.HasPrefix(value, p.Prefix) {
			return p, true
		}
	}
	return Provider{}, false
}

// Describe returns a format template for a credential value: the seed
// provider's when one recognizes it, and structural inference otherwise.
func Describe(value string) string {
	if p, ok := MatchProvider(value); ok {
		return p.Format
	}
	return Infer(value).String()
}

var (
	// prefixHead matches one leading alpha-started word followed by a single
	// separator, e.g. "sk-", "ghp_". The word is captured to gate entropy leaks.
	prefixHead = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]*)[-_]`)
	// structural delimiters split a value into independently-classified segments
	// while preserving the delimiter as a literal (e.g. JWT dot separators).
	structuralSplit = "."
)

// Infer returns a best-effort template describing value's shape. It never
// captures high-entropy characters as literals, so a sentinel generated from
// the result cannot leak bytes of the original value.
func Infer(value string) *Template {
	var parts []part
	for i, seg := range strings.Split(value, structuralSplit) {
		if i > 0 {
			parts = append(parts, part{literal: structuralSplit})
		}
		parts = append(parts, inferSegment(seg)...)
	}
	t := &Template{parts: parts, raw: render(parts)}
	t.re, _ = compileRegex(parts)
	return t
}

func inferSegment(seg string) []part {
	if seg == "" {
		return nil
	}
	if match := prefixHead.FindStringSubmatch(seg); match != nil {
		word := match[1]
		head := match[0] // word + separator
		remainder := seg[len(head):]
		if remainder != "" && prefixLike(word) {
			return []part{{literal: head}, randomPart(remainder)}
		}
	}
	return []part{randomPart(seg)}
}

// prefixLike reports whether a leading word is safe to keep as a literal: a
// short, wholly alphabetic scheme marker.
//
// Wholly alphabetic, not mostly: the separator this word ends at is only
// probably the provider's. A value whose random tail happens to contain an
// early "-" or "_" offers a longer word, and any of it kept as a literal is
// credential bytes written into a format that is then stored on the secret —
// which is the one thing this inference must never do. A marker with a digit
// in it is indistinguishable from entropy that starts with letters, so it is
// treated as entropy.
func prefixLike(word string) bool {
	if len(word) == 0 || len(word) > 10 {
		return false
	}
	for _, r := range word {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

func randomPart(seg string) part {
	return part{charset: classify(seg), length: len(seg)}
}

// classify returns the tightest charset name whose alphabet covers seg.
func classify(seg string) string {
	for _, name := range classifyOrder {
		if coversAll(charsets[name], seg) {
			return name
		}
	}
	return "base64"
}

func coversAll(alphabet, seg string) bool {
	for _, r := range seg {
		if !strings.ContainsRune(alphabet, r) {
			return false
		}
	}
	return true
}

func render(parts []part) string {
	var b strings.Builder
	for _, p := range parts {
		if p.charset == "" {
			b.WriteString(p.literal)
			continue
		}
		b.WriteString("{")
		b.WriteString(p.charset)
		b.WriteString(":")
		b.WriteString(strconv.Itoa(p.length))
		b.WriteString("}")
	}
	return b.String()
}
