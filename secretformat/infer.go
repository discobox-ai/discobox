package secretformat

import (
	"regexp"
	"strconv"
	"strings"
)

// Provider describes a known credential provider: how its keys are shaped and a
// default host binding hint.
type Provider struct {
	Name   string
	Prefix string // literal prefix used to recognize a value
	Format string // generative template
	Host   string // default host hint; empty means no default binding
}

// providers is the seed table, ordered longest-prefix-first so specific
// prefixes win over generic ones (e.g. sk-ant- before sk-).
var providers = []Provider{
	{Name: "anthropic", Prefix: "sk-ant-", Format: "sk-ant-{alnum:5}-{base64url:95}", Host: "api.anthropic.com"},
	{Name: "openai-project", Prefix: "sk-proj-", Format: "sk-proj-{base64url:48}", Host: "api.openai.com"},
	{Name: "openai", Prefix: "sk-", Format: "sk-{base64url:48}", Host: "api.openai.com"},
	{Name: "github-pat", Prefix: "github_pat_", Format: "github_pat_{base62:82}", Host: "api.github.com"},
	{Name: "github-token", Prefix: "ghp_", Format: "ghp_{base62:36}", Host: "api.github.com"},
	{Name: "github-oauth", Prefix: "gho_", Format: "gho_{base62:36}", Host: "api.github.com"},
	{Name: "slack-bot", Prefix: "xoxb-", Format: "xoxb-{digits:13}-{digits:13}-{base62:24}", Host: "slack.com"},
	{Name: "google-api", Prefix: "AIza", Format: "AIza{base64url:35}", Host: "www.googleapis.com"},
	{Name: "aws-access-key", Prefix: "AKIA", Format: "AKIA{base32:16}", Host: ""},
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

// Describe returns a format template and default host hint for a credential
// value, preferring the seed provider table and falling back to structural
// inference. The returned host may be empty.
func Describe(value string) (format string, host string) {
	if p, ok := MatchProvider(value); ok {
		return p.Format, p.Host
	}
	return Infer(value).String(), ""
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

// prefixLike reports whether a leading word is safe to keep as a literal: short
// and predominantly alphabetic, so it carries a scheme marker rather than
// credential entropy.
func prefixLike(word string) bool {
	if len(word) == 0 || len(word) > 10 {
		return false
	}
	alpha := 0
	for _, r := range word {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			alpha++
		}
	}
	return alpha*2 > len(word)
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
