// Package targetdb is the target database's contract (ADR 26-10-20-782 §6): what
// is known about a host, keyed by hostname, as input to fast-path generation
// and to the rewrite of a policy.
//
// An entry holds the host's documents — an OpenAPI description, a GraphQL
// schema, prose — named by their digest, the terms the host offers with their
// definitions, and the statement shapes it is known to support. An agent reads
// the documents as text; nothing here parses them. Lookup is code: a statement
// either has a known shape or it does not, and a miss is the caller's cue to
// read the documents.
package targetdb

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/hostscope"
	"github.com/discobox-ai/discobox/policy"
)

// Kind is what a document describes its host in.
type Kind string

const (
	// OpenAPI is an OpenAPI description of an HTTP API, JSON or YAML.
	OpenAPI Kind = "openapi"
	// GraphQL is a GraphQL schema in its schema definition language.
	GraphQL Kind = "graphql"
	// Prose is text a person wrote: how a protocol with no machine-readable
	// description works.
	Prose Kind = "prose"
)

// Kinds lists every document kind.
var Kinds = []Kind{OpenAPI, GraphQL, Prose}

// TermKind is which slot of a statement a term fills.
type TermKind string

const (
	// TermAction is an action word.
	TermAction TermKind = "action"
	// TermType is a resource type, a parent's type, or a scope's type.
	TermType TermKind = "type"
)

// TermKinds lists every term kind.
var TermKinds = []TermKind{TermAction, TermType}

// Bounds on what an entry may hold. GitHub's prototype vocabulary had 410
// words and 2,047 statement shapes.
const (
	MaxDocuments = 16
	// MaxDocumentBytes bounds one document. GitHub's REST description is
	// 13.5 MB.
	MaxDocumentBytes  = 32 << 20
	MaxDocumentName   = 64
	MaxTerms          = 4096
	MaxShapes         = 8192
	MaxShapeOperation = 256
)

// Entry is what is known about a host.
type Entry struct {
	// Host is the hostname the entry describes. It covers the hosts beneath
	// it, as a credential's host does (hostscope.Covers): github.com is also
	// api.github.com.
	Host      string     `json:"host"`
	Documents []Document `json:"documents"`
	// Terms are the words the host offers, which policies for it converge on.
	Terms []Term `json:"terms"`
	// Statements are the statement shapes the host is known to support,
	// generated from its documents: what the rewrite looks a statement up
	// among before it reads them. With the version, a shape whole — the
	// operations it covers included — is what generated rules are kept under
	// (§7). They are not part of the version, so a shape added or changed
	// later leaves the others' rules in place.
	Statements []Shape `json:"statements"`
}

// Document is one of an entry's documents, named by its digest. Its text is
// stored or fetched apart from the entry.
type Document struct {
	// Name is what the document is called in its entry, and in the path it
	// is read at.
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	// URL is where the text is fetched from, when it is not stored. It pins
	// a version — a commit, not a branch — and SHA256 is checked against
	// what it answers.
	URL string `json:"url,omitempty"`
	// SHA256 is the hex digest of the text.
	SHA256 string `json:"sha256"`
}

// Term is a word the host offers, and what it means.
type Term struct {
	Word       string   `json:"word"`
	Kind       TermKind `json:"kind"`
	Definition string   `json:"definition"`
}

// Shape is a statement with its names as parameters: `create on issue in
// repository {S}`. Its effect, names, conditions and creator are not part of
// it, since the rules that recognize a request are the same for any of them.
type Shape struct {
	Action       string `json:"action"`
	ResourceType string `json:"resourceType"`
	// Within are the parent types, outermost first.
	Within    []string `json:"within,omitempty"`
	ScopeType string   `json:"scopeType"`
	// ScopeWithin are the types the scope sits in, outermost first.
	ScopeWithin []string `json:"scopeWithin,omitempty"`
	// Operations are the documented operations the shape covers, as its
	// documents name them: `GET /repos/{owner}/{repo}/issues`, `mutation
	// createIssue`, `git-receive-pack`.
	Operations []string `json:"operations"`
}

// Key is the shape as one string, unique within an entry.
func (s Shape) Key() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s on %s", s.Action, s.ResourceType)
	if len(s.Within) > 0 {
		fmt.Fprintf(&b, " within %s", strings.Join(s.Within, ", "))
	}
	fmt.Fprintf(&b, " in %s", s.ScopeType)
	if len(s.ScopeWithin) > 0 {
		fmt.Fprintf(&b, " within %s", strings.Join(s.ScopeWithin, ", "))
	}
	return b.String()
}

var (
	hostLabel    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	documentName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	hexDigest    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// NormalizeHost is host as an entry is keyed by it.
func NormalizeHost(host string) string { return hostscope.Normalize(host) }

// ValidateHost reports whether host can key an entry: a DNS name, lowercase,
// with no port. An empty host would cover every host, and is refused.
func ValidateHost(host string) error {
	if host == "" {
		return errors.New("host is empty")
	}
	if host != NormalizeHost(host) {
		return fmt.Errorf("host %q is not lowercase, trimmed and without a port", host)
	}
	if len(host) > 253 {
		return fmt.Errorf("host %q is longer than 253 bytes", host)
	}
	for _, label := range strings.Split(host, ".") {
		if !hostLabel.MatchString(label) {
			return fmt.Errorf("host %q is not a DNS name", host)
		}
	}
	return nil
}

// Validate reports every way e is not an entry, joined, or nil.
func (e Entry) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if err := ValidateHost(e.Host); err != nil {
		fail("%w", err)
	}

	if len(e.Documents) > MaxDocuments {
		fail("an entry has at most %d documents, not %d", MaxDocuments, len(e.Documents))
	}
	names := map[string]bool{}
	for i, d := range e.Documents {
		for _, err := range d.validate() {
			fail("document %d: %w", i+1, err)
		}
		if names[d.Name] {
			fail("document %q is named twice", d.Name)
		}
		names[d.Name] = true
	}

	if len(e.Terms) > MaxTerms {
		fail("an entry has at most %d terms, not %d", MaxTerms, len(e.Terms))
	}
	offered := map[TermKind]map[string]bool{TermAction: {}, TermType: {}}
	for _, t := range e.Terms {
		for _, err := range t.validate() {
			fail("term %q: %w", t.Word, err)
		}
		words, ok := offered[t.Kind]
		if !ok {
			continue
		}
		if words[t.Word] {
			fail("term %q is a %s twice", t.Word, t.Kind)
		}
		words[t.Word] = true
	}

	if len(e.Statements) > MaxShapes {
		fail("an entry has at most %d statements, not %d", MaxShapes, len(e.Statements))
	}
	keys := map[string]bool{}
	for _, s := range e.Statements {
		key := s.Key()
		for _, err := range s.validate(offered) {
			fail("statement %q: %w", key, err)
		}
		if keys[key] {
			fail("statement %q is listed twice", key)
		}
		keys[key] = true
	}
	return errors.Join(errs...)
}

func (d Document) validate() []error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if len(d.Name) > MaxDocumentName || !documentName.MatchString(d.Name) {
		fail("name %q is not lowercase letters, digits, '.', '_' and '-', at most %d bytes", d.Name, MaxDocumentName)
	}
	if !slices.Contains(Kinds, d.Kind) {
		fail("kind %q is not one of %v", d.Kind, Kinds)
	}
	if !hexDigest.MatchString(d.SHA256) {
		fail("sha256 %q is not a lowercase hex SHA-256 digest", d.SHA256)
	}
	if d.URL != "" {
		if u, err := url.Parse(d.URL); err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			fail("url %q is not an https URL without credentials", d.URL)
		}
	}
	return errs
}

func (t Term) validate() []error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if err := policy.CheckWord(t.Word); err != nil {
		fail("%w", err)
	} else if policy.IsReserved(t.Word) {
		fail("%q means one thing everywhere and cannot be given a definition", t.Word)
	}
	if !slices.Contains(TermKinds, t.Kind) {
		fail("kind %q is not one of %v", t.Kind, TermKinds)
	}
	switch {
	case strings.TrimSpace(t.Definition) == "":
		fail("the definition is empty")
	case len(t.Definition) > policy.MaxTextBytes:
		fail("the definition is longer than %d bytes", policy.MaxTextBytes)
	case policy.HasControl(t.Definition):
		fail("the definition has a control character")
	}
	return errs
}

// validate reports what is wrong with s: a word its entry does not offer as
// the kind its slot takes, or a reserved word in a slot it does not fit.
func (s Shape) validate(offered map[TermKind]map[string]bool) []error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	switch {
	case s.Action == policy.ActionManage:
		// manage is every action on a type: a statement may say it, and
		// Lookup widens it to every shape of the type, but a shape is one
		// action's rules, and one stored as manage would answer no statement
		// of a concrete action.
		fail("action %q is every action, not one; a statement is one action's shape", s.Action)
	case policy.IsDefaultAction(s.Action):
	case policy.IsReserved(s.Action):
		fail("action %q is not an action", s.Action)
	case !offered[TermAction][s.Action]:
		fail("action %q is not a default and not one of the entry's actions", s.Action)
	}
	typeWord := func(what, w string) {
		switch {
		case policy.IsReserved(w):
			fail("%s %q names no one type", what, w)
		case !offered[TermType][w]:
			fail("%s %q is not one of the entry's types", what, w)
		}
	}
	typeWord("resource type", s.ResourceType)
	for _, w := range s.Within {
		typeWord("parent type", w)
	}
	typeWord("scope type", s.ScopeType)
	for _, w := range s.ScopeWithin {
		typeWord("scope parent type", w)
	}
	if len(s.Within) > policy.MaxParents || len(s.ScopeWithin) > policy.MaxParents {
		fail("a statement sits within at most %d parents", policy.MaxParents)
	}
	if len(s.Operations) == 0 {
		fail("a statement covers at least one documented operation")
	}
	if len(s.Operations) > MaxShapeOperation {
		fail("a statement covers at most %d operations, not %d", MaxShapeOperation, len(s.Operations))
	}
	for _, op := range s.Operations {
		switch {
		case strings.TrimSpace(op) == "":
			fail("an operation is empty")
		case len(op) > policy.MaxTextBytes:
			fail("an operation is longer than %d bytes", policy.MaxTextBytes)
		case policy.HasControl(op):
			fail("an operation has a control character")
		}
	}
	return errs
}

// Version names what e says about its host, so a decision can record which
// entry it used: the host, each document's name, kind and digest, and the
// terms. Where a document is fetched from is not part of it; what it says is.
// The statements are not either: they are the keys rules are kept under
// beside the version (§7), and a statement added to an entry must not orphan
// the rules every other statement already has.
func (e Entry) Version() string {
	documents := make([]Document, len(e.Documents))
	for i, d := range e.Documents {
		documents[i] = Document{Name: d.Name, Kind: d.Kind, SHA256: d.SHA256}
	}
	slices.SortFunc(documents, func(a, b Document) int { return strings.Compare(a.Name, b.Name) })
	terms := slices.Clone(e.Terms)
	slices.SortFunc(terms, func(a, b Term) int {
		return strings.Compare(string(a.Kind)+"\x00"+a.Word, string(b.Kind)+"\x00"+b.Word)
	})
	data, err := json.Marshal(Entry{Host: e.Host, Documents: documents, Terms: terms})
	if err != nil {
		// Every field is a string or a slice of them.
		panic(err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Document returns the document named name.
func (e Entry) Document(name string) (Document, bool) {
	i := slices.IndexFunc(e.Documents, func(d Document) bool { return d.Name == name })
	if i < 0 {
		return Document{}, false
	}
	return e.Documents[i], true
}

// Covers reports whether e describes host: its own host, or one beneath it.
func (e Entry) Covers(host string) bool {
	return e.Host != "" && hostscope.Covers(e.Host, host)
}
