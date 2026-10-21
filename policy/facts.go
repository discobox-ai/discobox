package policy

import (
	"maps"
	"slices"
	"strings"
)

// Facts is what the control plane recorded about a discobox when it was
// created, typed (§4, ADR 26-10-14-691): the record a policy's variables
// resolve from. Nothing in it can be moved by the discobox afterwards.
type Facts struct {
	Discobox Discobox `json:"discobox"`
	Sources  []Source `json:"sources,omitempty"`
}

// Discobox is the discobox a policy is bound to.
type Discobox struct {
	ID string `json:"id"`
	// CreatedBy is the ID of the discobox that created it, or empty when a
	// person did.
	CreatedBy string `json:"createdBy,omitempty"`
	Project   string `json:"project,omitempty"`
}

// Source is one source the discobox was created with, as recorded at create:
// never the branch or commits it has moved to since.
type Source struct {
	Slug    string `json:"slug"`
	Primary bool   `json:"primary,omitempty"`
	// Remote is nil for a source with no network remote.
	Remote *Remote `json:"remote,omitempty"`
	// Ref is the ref it was created from as its creator named it, or nil.
	Ref *Ref `json:"ref,omitempty"`
	// Commit is the commit it was created at, when one was recorded.
	Commit string `json:"commit,omitempty"`
}

// Remote is a source's network remote, normalized.
type Remote struct {
	// Host is the remote's host, lower-cased, with a port that is not the
	// scheme's default.
	Host string `json:"host"`
	// Repo is the repository's path on the host: owner/name on GitHub.
	Repo string `json:"repo"`
	URL  string `json:"url"`
}

// Ref is a ref a source was created from.
type Ref struct {
	// Type is "branch" or "tag", or empty when its creator named a ref
	// without saying which.
	Type string `json:"type,omitempty"`
	Name string `json:"name"`
}

// The variables a policy may use (§4), and nothing else.
const (
	// VarThis is the discobox the policy is bound to.
	VarThis = "$this"
	// VarPrimaryRepo, VarPrimaryHost and VarPrimaryBranch are its primary
	// source as recorded at create. The branch resolves only when the source
	// was created from a branch.
	VarPrimaryRepo   = "$primary.repo"
	VarPrimaryHost   = "$primary.host"
	VarPrimaryBranch = "$primary.branch"
	// VarPrimaryUpstream and VarPrimaryDefaultBranch are observed facts: the
	// repository the primary is a fork of, and that repository's default
	// branch. No observed fact is recorded yet, so they resolve to nothing.
	VarPrimaryUpstream      = "$primary.upstream"
	VarPrimaryDefaultBranch = "$primary.default_branch"
	// sourcesPrefix and sourceRepoSuffix bound $sources.<slug>.repo, another
	// source's repository as recorded.
	sourcesPrefix    = "$sources."
	sourceRepoSuffix = ".repo"
)

// SourceRepoVar is the variable for the repository of the source slug.
func SourceRepoVar(slug string) string { return sourcesPrefix + slug + sourceRepoSuffix }

// fixedVariables are the variables every discobox has, resolved or not, in
// the order Variables lists them. $sources.<slug>.repo is the one that is not
// fixed: there is one for each source with a slug.
var fixedVariables = []string{VarThis, VarPrimaryRepo, VarPrimaryHost, VarPrimaryBranch, VarPrimaryUpstream, VarPrimaryDefaultBranch}

// IsVariable reports whether name is a variable a policy may use.
func IsVariable(name string) bool {
	if slices.Contains(fixedVariables, name) {
		return true
	}
	slug, ok := sourceSlug(name)
	return ok && isSlug(slug)
}

func sourceSlug(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, sourcesPrefix)
	if !ok {
		return "", false
	}
	return strings.CutSuffix(rest, sourceRepoSuffix)
}

func isSlug(slug string) bool {
	if slug == "" || len(slug) > MaxWordBytes {
		return false
	}
	for _, r := range slug {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// Value is what variable resolves to against f, and whether it resolves at
// all. A variable with no value — no primary source, no network remote, a
// source not created from a branch, a fact nobody observed — matches nothing.
func (f Facts) Value(variable string) (string, bool) {
	switch variable {
	case VarThis:
		return nonEmpty(f.Discobox.ID)
	case VarPrimaryRepo, VarPrimaryHost:
		primary := f.primary()
		if primary == nil || primary.Remote == nil {
			return "", false
		}
		if variable == VarPrimaryHost {
			return nonEmpty(primary.Remote.Host)
		}
		return nonEmpty(primary.Remote.Repo)
	case VarPrimaryBranch:
		primary := f.primary()
		if primary == nil || primary.Ref == nil || primary.Ref.Type != "branch" {
			return "", false
		}
		return nonEmpty(primary.Ref.Name)
	}
	if slug, ok := sourceSlug(variable); ok && isSlug(slug) {
		for _, s := range f.Sources {
			if s.Slug == slug && s.Remote != nil {
				return nonEmpty(s.Remote.Repo)
			}
		}
	}
	return "", false
}

// Variable is one variable a policy may use and what it resolves to for a
// discobox: what an agent writing a policy is shown, so that a variable it
// writes means something it has seen.
type Variable struct {
	Name string `json:"name"`
	// Value is what it resolves to, and empty when Resolved is false.
	Value string `json:"value,omitempty"`
	// Resolved is false for a variable that resolves to nothing, and so
	// matches nothing.
	Resolved bool `json:"resolved"`
}

// Variables is every variable a policy may use for the discobox f describes,
// resolved or not, in a fixed order: $this, the $primary variables, then
// $sources.<slug>.repo for each source that has a slug, in source order. They
// are the values Resolve would record at approval.
func (f Facts) Variables() []Variable {
	names := slices.Clone(fixedVariables)
	for _, source := range f.Sources {
		if name := SourceRepoVar(source.Slug); IsVariable(name) && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	out := make([]Variable, 0, len(names))
	for _, name := range names {
		value, ok := f.Value(name)
		out = append(out, Variable{Name: name, Value: value, Resolved: ok})
	}
	return out
}

func (f Facts) primary() *Source {
	for i := range f.Sources {
		if f.Sources[i].Primary {
			return &f.Sources[i]
		}
	}
	return nil
}

func nonEmpty(value string) (string, bool) { return value, value != "" }

// Resolve is p as approved for the discobox f describes: Resolved holds the
// value of each variable p uses that resolves, and nothing else. A person
// approving p is shown these values; a variable missing from them matches
// nothing.
func (p Policy) Resolve(f Facts) Policy {
	resolved := map[string]string{}
	for _, variable := range p.Variables() {
		if value, ok := f.Value(variable); ok {
			resolved[variable] = value
		}
	}
	p.Statements = slices.Clone(p.Statements)
	p.Definitions = maps.Clone(p.Definitions)
	p.Resolved = nil
	if len(resolved) > 0 {
		p.Resolved = resolved
	}
	return p
}

// Binding is a variable as it stands for one decision: the value approved and
// the value the discobox's facts give now.
type Binding struct {
	Variable string
	// Approved is nil when the variable did not resolve at approval.
	Approved *string
	// Current is nil when it does not resolve now.
	Current *string
}

// Value is what the variable matches, and whether it matches anything: only
// when it resolved at approval and resolves now to that same value. A value
// that changed since a person approved it is not what they approved.
func (b Binding) Value() (string, bool) {
	if b.Approved == nil || b.Current == nil || *b.Approved != *b.Current {
		return "", false
	}
	return *b.Current, true
}

// Bind binds each variable p uses to the discobox f describes, at the time of
// a decision. A policy handed to another discobox binds there: f is that
// discobox's facts, and Resolved what was approved for it.
func (p Policy) Bind(f Facts) map[string]Binding {
	bound := make(map[string]Binding, len(p.Variables()))
	for _, variable := range p.Variables() {
		b := Binding{Variable: variable}
		if approved, ok := p.Resolved[variable]; ok {
			b.Approved = &approved
		}
		if current, ok := f.Value(variable); ok {
			b.Current = &current
		}
		bound[variable] = b
	}
	return bound
}

// Bound is p as it reads at one decision for the discobox f describes, for
// whatever reads a statement rather than matches it — a judge: each variable
// that binds (Bind) written as the value it matches, and the definitions of
// the words the statements left still use. Resolved is empty: nothing in it is
// a variable any more, or it did not bind.
//
// A variable that does not bind matches nothing. An allow that uses one covers
// nothing, so it is left out; a deny that uses one is kept with the variable as
// written, since keeping a deny only ever narrows what is allowed. A policy
// that allowed something may so allow nothing here.
func (p Policy) Bound(f Facts) Policy {
	var bound Policy
	bindings := p.Bind(f)
	for _, s := range p.Statements {
		unbound := false
		name := func(n string) string {
			if !IsVariable(n) {
				return n
			}
			if value, ok := bindings[n].Value(); ok {
				return value
			}
			unbound = true
			return n
		}
		namePtr := func(n *string) *string {
			if n == nil {
				return nil
			}
			v := name(*n)
			return &v
		}
		resources := func(in []Resource) []Resource {
			if in == nil {
				return nil
			}
			out := make([]Resource, len(in))
			for i, r := range in {
				out[i] = Resource{Type: r.Type, Name: namePtr(r.Name), CreatedBy: name(r.CreatedBy)}
			}
			return out
		}
		s.Actions = slices.Clone(s.Actions)
		s.Name = namePtr(s.Name)
		s.CreatedBy = name(s.CreatedBy)
		s.Within = resources(s.Within)
		s.Scope = Scope{Type: s.Scope.Type, Name: name(s.Scope.Name), Within: resources(s.Scope.Within)}
		if unbound && s.Effect != Deny {
			continue
		}
		bound.Statements = append(bound.Statements, s)
	}
	for _, word := range bound.Words() {
		if definition, ok := p.Definitions[word]; ok {
			if bound.Definitions == nil {
				bound.Definitions = map[string]string{}
			}
			bound.Definitions[word] = definition
		}
	}
	return bound
}
