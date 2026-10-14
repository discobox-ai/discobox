package judge

import (
	"fmt"
	"strings"

	"github.com/discobox-ai/discobox/sourcecache"
)

// Discobox is what the control plane recorded about a discobox a job
// concerns, when it was created (ADR 26-10-14-691). Every field is the
// control plane's record and none can be moved by the discobox afterwards:
// not its name, which is meta it writes, and not where its work has got to
// since. Its sources are what its creator named at create — a person, or the
// discobox that created it.
type Discobox struct {
	// SandboxID is the discobox's ID, the one name it cannot choose.
	SandboxID string
	// CreatedBy is the ID of the discobox that created it, or empty when a
	// person did. A person is not named: Discobox has no identity to state
	// for one yet.
	CreatedBy string
	// Primary is the source it was created with, or nil when it was created
	// with none.
	Primary *Source
	// Additional are its other sources, in the order they are stated. They
	// are stated last, so the cap on facts drops only them.
	Additional []Source
}

// Source is one of a discobox's sources as it was created: the remote and the
// checkout recorded at create, never the branch or commits it has moved to.
type Source struct {
	// Remote is the source's network remote as recorded, in any spelling git
	// accepts. It is normalized here, and a remote that names no repository on
	// a network is stated as none.
	Remote string
	// RefType and RefName are the ref it was created from, as a person named
	// it: a branch, a tag. Commit is the commit it was created at, stated
	// when no ref names it.
	RefType string
	RefName string
	Commit  string
}

// Facts is what the judge is told about the discobox asking, in Discobox's
// words, at most MaxFacts of them.
func Facts(asking Discobox) []string {
	return facts(subject{role: "The discobox asking", discobox: asking})
}

// DelegationFacts is what the judge is told about a delegation: the discobox
// handing the credential on, which is the one asking, and the discobox it
// would hand it to. Both are stated because a use that refers to "this
// discobox" means another one once it is handed on, and the judge can only
// see that from what each was created with.
func DelegationFacts(giving, receiving Discobox) []string {
	return facts(
		subject{role: "The discobox asking, which would hand the credential on,", discobox: giving},
		subject{role: "The discobox it would hand the credential to", discobox: receiving},
	)
}

// TargetFacts is what the judge is told about a request to the discobox API
// that names a discobox (ADR 26-10-14-320 §5): the discobox asking, and the
// one the request would act on. The target is stated because the call names
// it only by an ID, and a use bounded by who created a discobox ("the
// discoboxes this discobox created") can be read only from what was recorded
// when the target was. That no discobox created the target is stated, not
// left to the absence of a sentence: it is the fact such a use turns on.
func TargetFacts(asking, target Discobox) []string {
	return facts(
		subject{role: "The discobox asking", discobox: asking},
		subject{role: "The discobox the request names, which it would act on,", discobox: target, statesNoCreator: true},
	)
}

// subject is one discobox a job's facts describe, and the words that say
// which it is.
type subject struct {
	role     string
	discobox Discobox
	// statesNoCreator says that no discobox created it when none did, rather
	// than saying nothing.
	statesNoCreator bool
}

// facts states each subject: who it is, its primary source and its creator,
// and then the other sources of every subject while the cap allows. Every
// sentence names the discobox it is about, so facts about two read apart. A
// discobox whose other sources do not all fit is said to have more than are
// listed, so none of them is left out unremarked; the slot that says so is
// kept for each subject before any is filled. A discobox with no ID says
// nothing: a fact about a discobox that cannot be named would be about nobody.
func facts(subjects ...subject) []string {
	var out []string
	named := subjects[:0:0]
	for _, s := range subjects {
		id := strings.TrimSpace(s.discobox.SandboxID)
		if id == "" {
			continue
		}
		named = append(named, s)
		out = append(out, fmt.Sprintf("%s is %s.", s.role, id))
		switch primary := s.discobox.Primary; {
		case primary != nil:
			out = append(out, fmt.Sprintf("The primary source %s was created with is %s.", id, primary.describe()))
		case len(s.discobox.Additional) > 0:
			out = append(out, fmt.Sprintf("%s was created with no primary source.", id))
		default:
			out = append(out, fmt.Sprintf("%s was created with no source.", id))
		}
		if creator := strings.TrimSpace(s.discobox.CreatedBy); creator != "" {
			out = append(out, fmt.Sprintf("%s was created by the discobox %s.", id, creator))
		} else if s.statesNoCreator {
			out = append(out, fmt.Sprintf("%s was created by no discobox.", id))
		}
	}
	// One slot each is held back for the subjects after this one that have
	// other sources, so the last can still say what was not listed.
	reserved := 0
	for _, s := range named {
		if len(s.discobox.Additional) > 0 {
			reserved++
		}
	}
	for _, s := range named {
		additional := s.discobox.Additional
		if len(additional) == 0 {
			continue
		}
		reserved--
		id := strings.TrimSpace(s.discobox.SandboxID)
		room := MaxFacts - len(out) - reserved
		listed := len(additional)
		if listed > room {
			listed = max(room-1, 0)
		}
		for _, source := range additional[:listed] {
			out = append(out, fmt.Sprintf("Another source %s was created with is %s.", id, source.describe()))
		}
		if unlisted := len(additional) - listed; unlisted > 0 {
			out = append(out, fmt.Sprintf("%s was created with %d more sources than are listed here.", id, unlisted))
		}
	}
	return out
}

// describe is a source as a fact states it.
func (s Source) describe() string {
	what := "a Git repository with no network remote"
	// A remote on a pool's origins host resolves only inside the discobox
	// that has it, and names no repository a request could reach. One a fact
	// could not repeat as a word names nothing it can state.
	if remote := factWord(sourcecache.NormalizeRemote(s.Remote)); remote != "" && !sourcecache.OnPoolOriginsHost(s.Remote) {
		what = "the Git repository " + remote
	}
	refName, commit := factWord(s.RefName), strings.ToLower(strings.TrimSpace(s.Commit))
	if !isCommit(commit) {
		commit = ""
	}
	switch refType := strings.TrimSpace(s.RefType); {
	case refName != "" && (refType == "branch" || refType == "tag"):
		return fmt.Sprintf("%s, checked out from the %s %s", what, refType, refName)
	case refName != "":
		return fmt.Sprintf("%s, checked out from %s", what, refName)
	case commit != "":
		return fmt.Sprintf("%s, checked out at the commit %s", what, commit)
	default:
		return what
	}
}

// maxFactWord is the longest remote or ref name a fact repeats.
const maxFactWord = 200

// factWord is a remote or ref name as a fact may repeat it, or empty. Whoever
// created the discobox named it, and a fact is Discobox speaking, so it is
// repeated only when it is one printable ASCII word: no whitespace, no control
// characters, nothing a sentence could be built out of, and nothing JSON has to
// escape.
func factWord(word string) string {
	word = strings.TrimSpace(word)
	if word == "" || len(word) > maxFactWord {
		return ""
	}
	for _, r := range word {
		// JSON writes <, > and & six bytes long, and " and \ two: a word
		// repeated in a fact costs the job what it looks like it costs.
		if r <= ' ' || r == 0x7f || r > 0x7e || strings.ContainsRune(`<>&"\`, r) {
			return ""
		}
	}
	return word
}

// isCommit reports whether value is a commit ID: hexadecimal, SHA-1 or
// SHA-256 length or an abbreviation of one.
func isCommit(value string) bool {
	if len(value) < 7 || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
