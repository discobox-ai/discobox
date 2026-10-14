package judge

import "strings"

// Recognition names what trusted code recognized a request, or its body, as:
// a protocol, an endpoint, or the parser that read the body (ADR 26-09-26-240
// §1, §6). The version changes whenever what that code shows or says changes,
// so a stored verdict can be read against the code that described it.
type Recognition struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// The protocols a pool recognizes. A protocol is the wire protocol, whatever
// host speaks it.
const (
	// ProtocolGitReceivePack is a git push over smart HTTP: a POST to
	// …/git-receive-pack whose body is the ref updates, as pkt-lines, then a
	// packfile.
	ProtocolGitReceivePack = "git-receive-pack"
)

// The endpoints a pool recognizes. An endpoint is one operation on one API.
const (
	// EndpointGitHubFork is GitHub's POST /repos/{owner}/{repo}/forks.
	EndpointGitHubFork = "github.fork"
	// EndpointDiscoboxSandboxCreate is the discobox API's
	// POST /projects/{project}/sandboxes, reached through a pool's gate.
	EndpointDiscoboxSandboxCreate = "discobox.sandbox.create"
	// EndpointDiscoboxSandboxGet is the discobox API's
	// GET /projects/{project}/sandboxes/{sandbox}: reading one discobox back,
	// which `discobox new` polls after a create.
	EndpointDiscoboxSandboxGet = "discobox.sandbox.get"
	// EndpointDiscoboxSandboxOriginRefs is git's discovery of a discobox's
	// origin repository before a push to it:
	// GET /projects/{project}/sandboxes/{sandbox}/git-origins/{repo}/info/refs.
	EndpointDiscoboxSandboxOriginRefs = "discobox.sandbox.origin.refs"
	// EndpointDiscoboxSandboxOriginPush is a push into a discobox's origin
	// repository, which is how `discobox new` delivers a created discobox's
	// source: POST …/git-origins/{repo}/git-receive-pack.
	EndpointDiscoboxSandboxOriginPush = "discobox.sandbox.origin.push"
	// EndpointDiscoboxSandboxSourcePushed is the report that a discobox's
	// sources are pushed, which lets it start:
	// POST /projects/{project}/sandboxes/{sandbox}/complete-source-push.
	EndpointDiscoboxSandboxSourcePushed = "discobox.sandbox.source-pushed"
)

// operationOutsideBody names the endpoints whose operation is their method and
// path alone: a read with nothing in its body to say. An allow for one may
// stand for its route (Request.OperationInBody). Every other endpoint — a
// create, a push, a report, or one this package does not know — is read as
// carrying its operation in its body, which is the reading that never lets a
// body go unjudged.
var operationOutsideBody = map[string]bool{
	EndpointDiscoboxSandboxGet:        true,
	EndpointDiscoboxSandboxOriginRefs: true,
}

// The parsers a pool reads a body with, chosen by media type and refined by
// the protocol.
const (
	ParserText           = "text"
	ParserJSON           = "json"
	ParserForm           = "form"
	ParserMultipart      = "multipart"
	ParserGitReceivePack = ProtocolGitReceivePack
)

// guidance is what Discobox knows about a protocol or an endpoint, as the
// judge is told it: trusted sentences about what the operation means and what
// to weigh (ADR 26-09-26-240 §4). It is here, on the trusted side, and not in
// what a pool sends: a pool names what it recognized, and the words that go
// with that name are the judge package's.
//
// Guidance explains. It never authorizes: the approved purpose and host are
// the only authorization, and nothing here widens them.
var guidance = map[string][]string{
	ProtocolGitReceivePack: {
		"This is a git push over smart HTTP. The body's metadata lists the refs the push changes, read the way the server reads them: \"create\" makes a ref that does not exist yet, \"update\" moves one from its old commit to a new one, and \"delete\" removes one. When the list is all of them, the push changes those refs and nothing else. It is not all of them when \"commandsCut\" says the body was longer than Discobox read, or when \"commandsTotal\" is larger than the list; and with no metadata, \"parseError\" says why the body could not be read, and what the push changes is unknown.",
		"A body whose metadata is only \"probe\" is git asking whether the server will take a push too large to buffer. It changes nothing; the push itself follows as its own request.",
		"Force is not on the wire. `git push --force` only lets a client send an update that is not a fast-forward, and whether an update is a fast-forward depends on the repository's history, which is not in the request. A create or a delete is never a forced update. An update's metadata cannot say whether it rewrites history; weigh that only when the approved purpose rules out rewriting a ref that already exists.",
		"The packfile after the ref updates holds the objects the new commits need. It is binary, and is described by its size rather than shown.",
	},
	EndpointGitHubFork: {
		"This is GitHub's fork endpoint: it creates a copy of the repository named in the path. The body's \"organization\" is where the copy is created; with none, it is created in the account the credential belongs to. \"name\" renames the copy, and \"default_branch_only\" copies only the default branch.",
	},
	EndpointDiscoboxSandboxCreate: {
		"This creates one new discobox: a sandbox that runs an agent as the same user, on the \"prompt\" it is given, starting from the \"source\" and any \"otherSources\" named (\"none\" is an empty machine). One request is one discobox.",
		"The discobox CLI's `discobox new` sends this request: its prompt (`-p`) is \"prompt\", each `--grant` is one of \"grants\", and the directory it runs in is \"source\". A purpose approved for creating discoboxes is often worded as that command, or as which discoboxes to create, and may quote or paraphrase the prompt each is given — even one that tells the new discobox what not to do. That quoted task is the new discobox's to carry out: it says which discoboxes the purpose approves creating, not what the requester is limited to doing. Weigh whether this prompt is one the purpose names — the same task, for one of the items the purpose lists — and whether its grants are within what the purpose delegates.",
		"Each of \"grants\" hands the new discobox uses of one credential — \"credential\" for a well-known one, or \"secret\" and \"envVar\" for a project secret — to \"host\" when named, for \"ttlSeconds\" when named. Its \"uses\" are the sentences the new discobox's own commands and requests will be judged against, so they are what it may do with the credential: weigh whether each is within what the approved purpose delegates, as narrow as the purpose says. \"secrets\" are credentials assigned to it outright, with no use sentence to judge against, which is broader than a grant. \"env\" names plain environment variables it sets; their values are not in the metadata and may themselves be a credential, so ask for the body when a name suggests one.",
		"\"prompt\" is only the start of what the new discobox is told, and \"promptBytes\" says how long it is; it is the discobox's task in the requester's words, not an authorization. A prompt that ends in \"…\" was cut short to fit, and says nothing about the grants. The metadata lifts what a create most often turns on, not every field (the harness config, the model, the user and the git identity are left to the body).",
		"\"grantsTotal\" is how many grants the body holds, 0 when it holds none, and absent when its grants could not be read. Every grant is in the metadata when \"grants\" lists that many and none of their \"uses\" ends in \"…\". When \"grantsTotal\" is larger or absent, or a use was cut short, ask for the body to see the rest rather than refusing for want of it.",
		"After the create, `discobox new` finishes making the discobox in requests of their own, each recognized as such: it reads the new discobox back until it is ready for its source, pushes the source into the discobox's origin, and reports the push done.",
	},
	EndpointDiscoboxSandboxGet: {
		"This reads one discobox back, the one named in the path: its configuration and how far it is in starting. It changes nothing. `discobox new` polls it about once a second after creating a discobox, until that discobox is ready to receive its source, and `discobox admin box get` reads it too. A purpose that approves creating discoboxes approves reading back the ones it creates.",
	},
	EndpointDiscoboxSandboxOriginRefs: {
		"This is git asking what the origin repository of the discobox named in the path holds, before pushing to it. It changes nothing. It is the first half of `discobox new` delivering the source of a discobox it created. The facts say which discobox created the one named.",
	},
	EndpointDiscoboxSandboxOriginPush: {
		"This pushes into the origin repository (\"git-origins\") that the discobox named in the path checks its source out from. It is how `discobox new` delivers the source of a discobox it has just created — the commit that discobox was created from — before it starts: a discobox getting its code, not a push to any upstream repository such as GitHub. A purpose that approves creating discoboxes approves delivering the source of the ones it creates, even when the prompt it quotes tells the new discobox not to push, and the facts say which discobox created the one named: a push into one it did not create puts commits where another discobox's agent will take them as its newest.",
	},
	EndpointDiscoboxSandboxSourcePushed: {
		"This reports that the sources of the discobox named in the path have been pushed, so it can start: the last step of `discobox new` delivering the source of a discobox it created. Its body names each source and the commit pushed for it. A purpose that approves creating discoboxes approves this for the ones it creates, which the facts say.",
	},
}

// GuidanceFor is the guidance for what a request was recognized as: its
// protocol's, then its endpoint's. A name this package does not know brings
// none: a pool that recognizes something its control plane has no words for
// is judged without them rather than refused.
func GuidanceFor(request *Request) []string {
	if request == nil {
		return nil
	}
	var out []string
	for _, recognized := range []*Recognition{request.Protocol, request.Endpoint} {
		if recognized != nil {
			out = append(out, guidance[recognized.Name]...)
		}
	}
	return out
}

// valid reports whether a recognition names something in the form every
// name here takes: short, lower case, and nothing but letters, digits, dots
// and dashes. What a pool recognized is recorded and shown; it is not free
// text.
func (r *Recognition) valid() bool {
	if r == nil {
		return true
	}
	if r.Name == "" || len(r.Name) > 64 || r.Version < 1 {
		return false
	}
	return strings.Trim(r.Name, "abcdefghijklmnopqrstuvwxyz0123456789.-") == ""
}
