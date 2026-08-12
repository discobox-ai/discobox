// Package sandboxuser owns the identity a sandbox process runs as: the type
// that carries it, the precedence between the layers that describe it, and the
// vocabulary a caller uses to say which parts of it that caller actually needs.
//
// It performs no lookups. Completing an identity means reading the image's own
// /etc/passwd and /etc/group, which only code inside the sandbox can do, so
// completion lives in sandbox-agent/runuser and this package is the half that
// is safe everywhere else. The split is load-bearing rather than tidy: the pool
// agent imports this module and cannot import sandbox-agent, so "the host must
// not resolve" (ADR 0025 §4) is enforced by the build graph instead of by a
// rule someone has to remember (ADR 0033 §4).
package sandboxuser

import "strings"

// User is the identity a process runs as. It is one type across the API, the
// manifest, the pool agent, and the launch path, so a field cannot mean one
// thing at one layer and something else at the next (ADR 0025 §1).
//
// A nil *User named nobody. Within a User, every field is independently
// optional and nil means absent -- never zero, which for a uid is root and for
// a gid is the root group (ADR 0033 §3).
type User struct {
	Name string `json:"name,omitempty"`
	UID  *int64 `json:"uid,omitempty"`
	GID  *int64 `json:"gid,omitempty"`
	// GroupName is the primary group by name, mutually exclusive with GID.
	// Resolution turns it into GID and clears it, so nothing downstream has to
	// know which of the two a caller supplied.
	GroupName     string `json:"groupName,omitempty"`
	HomeDirectory string `json:"homeDirectory,omitempty"`
	// AdditionalGroups are supplementary groups, each a group name or a numeric
	// GID. Whoever supplied the list is the authority on membership; the group
	// file is consulted only to resolve an entry to an id (ADR 0025 §3).
	AdditionalGroups []string `json:"additionalGroups,omitempty"`
}

// Fields names the parts of an identity a caller requires. A caller passes the
// set it genuinely needs; anything required but undeterminable is an error
// naming the field, and anything not required comes back absent rather than
// defaulted (ADR 0033 §2).
//
// Leaving a field out is how a caller says "I know I cannot have this here" --
// an explicit, greppable claim, rather than a zero value that reads like an
// answer everywhere downstream.
type Fields uint8

const (
	FieldUID Fields = 1 << iota
	FieldGID
	FieldName
	FieldHome
	FieldGroups
)

// Credential is what a launch path must have before it can call setuid: both
// ids and the supplementary set. Name and home are not needed to become
// somebody, only to describe them.
const Credential = FieldUID | FieldGID | FieldGroups

// Complete is every field, for callers that build a process environment
// (USER, LOGNAME, HOME) as well as its credential.
const Complete = Credential | FieldName | FieldHome

// Has reports whether every field in want is present in f.
func (f Fields) Has(want Fields) bool { return f&want == want }

// String names the fields in f, for error messages that have to say which part
// could not be resolved.
func (f Fields) String() string {
	var out []string
	for _, named := range []struct {
		field Fields
		name  string
	}{
		{FieldUID, "uid"},
		{FieldGID, "gid"},
		{FieldName, "name"},
		{FieldHome, "home directory"},
		{FieldGroups, "additional groups"},
	} {
		if f.Has(named.field) {
			out = append(out, named.name)
		}
	}
	if len(out) == 0 {
		return "no fields"
	}
	return strings.Join(out, ", ")
}

// Layers are the descriptions of who a process should run as, ordered from
// most general to most specific. A nil layer named nobody.
type Layers struct {
	// Image is who the process already is: the account behind the Dockerfile's
	// USER directive, as it exists in the image's own /etc/passwd. Only code
	// inside the sandbox can supply it.
	Image *User
	// Manifest is the sandbox's declared user, from sandbox.json.
	Manifest *User
	// Request is a single call's override, from an exec or terminal create.
	Request *User
}

// Merge applies precedence and nothing else. It performs no lookups, so it
// cannot complete an identity and therefore cannot guess at one; callers inside
// the sandbox pass the result to runuser.Resolve, which finishes the job
// against the image's own account database.
//
// An identity has three facets, and each is chosen whole from the most specific
// layer that names it:
//
//   - Who to run as: name, uid, home directory.
//   - The primary group: gid or group name.
//   - Supplementary groups.
//
// Choosing each facet whole is what makes a partial request expressible. "The
// usual user, but in group docker" and "the usual user, plus these groups" both
// say something about one facet and nothing about the others, and each used to
// be mishandled in its own way -- a named group was dropped in silence, a
// numeric one failed with "uid is required", because a single all-or-nothing
// test decided all three at once.
//
// The primary group is the one facet that does not outlive the identity above
// it: a layer that names who to run as also decides which layers may still
// answer for that user's primary group. Inheriting a gid across a change of
// identity would run user A's process in user B's default group, which is the
// uid/gid confusion ADR 0025 §6 exists to prevent. Supplementary groups do
// cross that boundary deliberately -- they describe what the *sandbox* may
// reach rather than who it is, so naming a user must not silently strip them
// (ADR 0025 §2).
func Merge(l Layers) User {
	// Most specific first, which is the order every loop below wants.
	layers := []*User{l.Request, l.Manifest, l.Image}

	var out User
	identity := len(layers) // index of the layer that supplied the identity
	for i, layer := range layers {
		if !NamesIdentity(layer) {
			continue
		}
		out.Name = strings.TrimSpace(layer.Name)
		out.UID = cloneID(layer.UID)
		out.HomeDirectory = strings.TrimSpace(layer.HomeDirectory)
		identity = i
		break
	}

	// At or above the identity's layer only: see the doc comment.
	for i := 0; i <= identity && i < len(layers); i++ {
		if !NamesPrimaryGroup(layers[i]) {
			continue
		}
		out.GID = cloneID(layers[i].GID)
		out.GroupName = strings.TrimSpace(layers[i].GroupName)
		break
	}

	for _, layer := range layers {
		if !NamesGroups(layer) {
			continue
		}
		out.AdditionalGroups = append([]string(nil), layer.AdditionalGroups...)
		break
	}
	return out
}

// NamesIdentity reports whether a layer says who to run as.
//
// This is the predicate that decides whether a layer is answered or fallen
// through, and it exists exactly once on purpose. It used to be written
// per-site: boot asked one question, execs asked a subtly different one, and
// the difference was invisible until an exec naming only a group ran as the
// wrong one. A field added to User is taught to this function, not to five
// call sites that each have to remember (ADR 0033 §1).
func NamesIdentity(u *User) bool {
	return u != nil && (strings.TrimSpace(u.Name) != "" ||
		u.UID != nil ||
		strings.TrimSpace(u.HomeDirectory) != "")
}

// NamesPrimaryGroup reports whether a layer chooses a primary group, by either
// of the two ways of spelling one.
func NamesPrimaryGroup(u *User) bool {
	return u != nil && (u.GID != nil || strings.TrimSpace(u.GroupName) != "")
}

// NamesGroups reports whether a layer chooses the supplementary set. An empty
// list is not a choice: groups are all-or-nothing, so "none named" inherits and
// only a non-empty list replaces (ADR 0025 §2).
func NamesGroups(u *User) bool {
	return u != nil && len(u.AdditionalGroups) > 0
}

// Named reports whether a layer says anything at all about identity. A layer
// naming nobody is indistinguishable from an absent one.
func Named(u *User) bool {
	return NamesIdentity(u) || NamesPrimaryGroup(u) || NamesGroups(u)
}

// Validate reports contradictions inside a single layer, which no amount of
// merging can settle. It is checkable without an account database, so the
// control plane and the pool agent reject a malformed request at the edge
// rather than passing it inward to fail at launch.
func (u *User) Validate() error {
	if u == nil {
		return nil
	}
	if u.GID != nil && strings.TrimSpace(u.GroupName) != "" {
		return errBothGIDAndGroupName
	}
	return nil
}

// Clone deep-copies a user, trimming the string fields so that whitespace
// cannot make an absent field look present to Named.
func (u *User) Clone() *User {
	if u == nil {
		return nil
	}
	out := *u
	out.Name = strings.TrimSpace(out.Name)
	out.GroupName = strings.TrimSpace(out.GroupName)
	out.HomeDirectory = strings.TrimSpace(out.HomeDirectory)
	out.UID = cloneID(u.UID)
	out.GID = cloneID(u.GID)
	out.AdditionalGroups = append([]string(nil), u.AdditionalGroups...)
	return &out
}

func cloneID(in *int64) *int64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

// ID returns a pointer to v, for building a User from known ids.
func ID(v int64) *int64 { return &v }
