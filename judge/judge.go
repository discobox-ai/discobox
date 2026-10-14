// Package judge is the question Discobox puts to a model before a credential
// is used, and the answer it will accept back (ADR 26-09-22-838).
//
// It holds the contract and nothing that runs it: the job a caller may ask,
// the system prompt and schema the trusted side supplies, and the strict
// decoding of what comes back. A caller never chooses the prompt, the model,
// the tools, or the schema — those are here, so that what is asked of the
// judge is the same wherever the asking happens.
package judge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// Role is what the harness's discobox-prompt is asked for. It names a role
	// rather than a vendor's model: which model answers is the image's
	// decision, and nothing here learns one.
	Role = "judge"
	// PromptVersion changes whenever System changes. A stored verdict names
	// it, so a decision can be read against the words that produced it.
	PromptVersion = "11"
	// Timeout bounds one exchange — every round of it together, not each ask.
	// A request is being held open while the judge thinks.
	//
	// The caller owns it, because only the caller knows an exchange is under
	// way: it passes a context carrying the remaining time, and each ask is
	// bounded by whichever is sooner, that or this. A judge runtime applies it
	// on its own as a ceiling for a caller that passed no deadline at all,
	// which bounds one ask rather than the exchange.
	Timeout = 90 * time.Second
	// ReachWait is how long the control plane waits for the judge's discobox
	// to become reachable before putting the job to it. A server that has just
	// started trusts no pool until the pool's next heartbeat, and a pool that
	// has just restarted is not ready until its agent is, so a judge that is
	// about to be reachable is waited on rather than refused. It is spent
	// before Timeout starts, and every hop that bounds the whole exchange
	// allows for both.
	ReachWait = 60 * time.Second
	// MaxRounds is how many times the judge may be asked about one job: the
	// first ask, and at most two more after it asks to be shown something.
	// A judge still asking on the last round has decided nothing, and a job
	// that decides nothing is refused.
	MaxRounds = 3
	// MaxInput keeps one job's JSON below the per-argument exec limit, since
	// the prompt reaches the wrapper as an argument.
	MaxInput = 64 << 10
	// MaxOutput bounds what a wrapper may hand back. A verdict is two short
	// fields; anything larger is a transcript or a runaway.
	MaxOutput = 64 << 10
	// MaxBodyBytes is the most of a request body the judge may be shown, and
	// the cap on what it may ask for.
	//
	// It is far below MaxInput because a body is shown inside the job's JSON,
	// where one byte can become six: a control character is written \u0000,
	// and a body that is not text is escaped character by character. At 8 KiB
	// even that worst case is around 48 KiB, which leaves the rest of the job
	// — the approved use, the destination, the headers — inside MaxInput. Do
	// not raise it by dividing MaxInput by six: the room the rest of the job
	// needs is the other half of the sum, and a cap Validate then refuses is
	// a body the judge asked for and cannot be shown. Validate is the backstop
	// either way, since a job is refused whole rather than sent truncated.
	MaxBodyBytes = 8 << 10
	// MaxMetadataBytes is the most a parser may say about a body, as JSON.
	// Metadata is in every ask, the first included, so it is held to a small
	// fraction of what a body shown on request may be: it is what the
	// operation is, not what it carries (ADR 26-09-26-240 §3).
	MaxMetadataBytes = 2 << 10
	// MaxReportedBytes is the most any one thing a discobox reports about
	// where a command runs may be. They are a path or a line, and anything
	// longer is not that. It is agentcreds.MaxReportedBytes, which the
	// discobox cuts them to, and small for the reason that is: escaped at its
	// worst beside the largest input shown, a job still fits MaxInput.
	MaxReportedBytes = 256
	// MaxFacts is the most the control plane may say about one job. Facts
	// are short and specific; a long list is a context the judge drowns in.
	MaxFacts = 8
)

// The kinds of job. Each is judged against one approved use.
const (
	// KindCommand is a command a discobox is about to run under an approved
	// use, judged before any credential is minted for it.
	KindCommand = "command"
	// KindRequest is a request observed by the proxy, judged before the
	// credentials it carries are resolved.
	KindRequest = "request"
	// KindDelegation is a discobox about to hand a credential on by
	// approving another discobox's request, judged before the grant is
	// minted: whether the uses it would hand on fall within the uses it was
	// delegated (ADR 26-09-30-782 §3). Both are authorization, and there is no
	// request evidence.
	KindDelegation = "delegation"
	// KindRule is whether what a discobox asked a credential for falls within
	// the uses of an approval rule, judged before the rule approves the
	// request (ADR 26-10-12-330 §3). It is a delegation job's shape with its
	// own words, because who wrote what differs: the rule's uses are a
	// person's and name a class, and the uses asked for are an agent's.
	KindRule = "rule"
)

// Job is one question, and everything the judge is allowed to see in order to
// answer it. Purpose and Host are the authorization — the sentence a person
// approved and where that credential may go. Everything else is evidence, and
// evidence is data to weigh, never instructions to follow.
type Job struct {
	Kind string `json:"kind"`
	// Purpose is the approved use, in the words it was approved in.
	Purpose string `json:"purpose"`
	// Host is the host the use was approved for.
	Host string `json:"host"`
	// Credential names the credential in the words a person reads, never its
	// value.
	Credential string `json:"credential,omitempty"`
	// Round is which ask this is, from 1. A round after the first exists
	// because the judge asked to be shown the body.
	Round int `json:"round"`
	// BodyCanBeShown and AsksLeft say, beside the request, that the judge may
	// still ask to be shown its body and how many more times it may ask. They
	// are derived by Prompt, never supplied: a caller's values are replaced.
	// The rule to ask rather than refuse is in System, but a model deciding
	// one request follows what the request in front of it says it can do.
	BodyCanBeShown bool `json:"bodyCanBeShown,omitempty"`
	AsksLeft       int  `json:"asksLeft,omitempty"`
	// Command is the argv, for a command job. For a request job it is what
	// the discobox declared it was running, which is context and not
	// authority: a declaration cannot override the request observed.
	Command []string `json:"command,omitempty"`
	// Stdin is what a command job's command will read on standard input, as
	// much of it as the discobox showed (ADR 26-09-27-905). For a command
	// that takes its request there, it is the operation.
	Stdin *Input `json:"stdin,omitempty"`
	// Reported is what the discobox said about where a command job's command
	// runs (ADR 0090). It is the discobox's claim, written on the side being
	// judged, and never a fact the trusted side established.
	Reported *Reported `json:"reported,omitempty"`
	// Request is the request observed by the proxy, for a request job.
	Request *Request `json:"request,omitempty"`
	// Uses are the uses a discobox is about to hand on, for a delegation job:
	// what it would grant, judged against Purpose, which is what it was
	// delegated. For a rule job they are the uses a discobox asked for, judged
	// against Purpose, which is the rule's uses.
	Uses []string `json:"uses,omitempty"`
	// Guidance is what Discobox knows about what the request was recognized
	// as (GuidanceFor), set by the trusted side that builds the job and never
	// by whoever observed the request (ADR 26-09-26-240 §4). It explains; it
	// never authorizes.
	Guidance []string `json:"guidance,omitempty"`
	// Facts are what the control plane recorded about the discoboxes a job
	// concerns, in its own words (Facts and DelegationFacts,
	// ADR 26-10-14-691): the discobox asking — its ID, the sources it was
	// created with, the discobox that created it — and, for a delegation, the
	// one it would hand the credential to. Like Guidance
	// they are the trusted side's, set by what builds the job and never by a
	// pool, and they are facts rather than authority: a fact explains what
	// the purpose refers to, and never widens it.
	Facts []string `json:"facts,omitempty"`
}

// Input is what a command will read on standard input, as the discobox
// showed it: text, and a sentence for whatever of it is not shown.
type Input struct {
	// Content is the input shown, which is text. It is empty when none of it
	// could be shown, and Missing then says why.
	Content string `json:"content"`
	// Missing says, in a sentence for the judge, why Content is not the whole
	// input: longer than may be shown, still arriving, not text, or a read
	// that failed. It is said rather than hidden, as a body's is.
	Missing string `json:"missing,omitempty"`
}

// Reported is what a discobox says about where a command runs. Every field is
// optional, and every one is a claim: the discobox under judgement wrote it.
type Reported struct {
	// WorkingDirectory is the directory the command runs in.
	WorkingDirectory string `json:"workingDirectory,omitempty"`
	// RepositoryRoot is the root of the git checkout it runs in.
	RepositoryRoot string `json:"repositoryRoot,omitempty"`
	// RefCommit and RefSubject are the commit a git ref the command names
	// resolves to, and that commit's subject line: the agent's own words
	// about its work, reaching the judge by a second route (ADR 0090 §3).
	RefCommit  string `json:"refCommit,omitempty"`
	RefSubject string `json:"refSubject,omitempty"`
}

// Request is a request as the proxy saw it, before any credential was
// substituted into it and with every credential-bearing value redacted.
type Request struct {
	Method string `json:"method"`
	// URL is the destination authority and port, path, and query.
	URL string `json:"url"`
	// Headers are the headers worth weighing, redacted.
	Headers map[string][]string `json:"headers,omitempty"`
	// Protocol and Endpoint are what trusted code recognized the request as,
	// when it recognized it (ADR 26-09-26-240 §1). Each brings its guidance.
	Protocol *Recognition `json:"protocol,omitempty"`
	Endpoint *Recognition `json:"endpoint,omitempty"`
	// Body describes the request's body, and carries it once the judge has
	// asked for it. A request with no body has none.
	Body *Body `json:"body,omitempty"`
}

// Body is what the judge is told about a request's body, in the one shape it
// is always told it in (ADR 26-09-26-240 §2).
//
// The first ask describes it: its media type and length, and — when a parser
// recognized it — what that parser found worth knowing, which is small and is
// what the operation is. The body's own bytes are shown only when the judge
// asks for them, since most requests are decided by what they are and where
// they go, and a body sent every time is tokens spent answering a question
// already settled.
type Body struct {
	// MediaType is the content type as declared, if it was.
	MediaType string `json:"mediaType,omitempty"`
	// Length is how many bytes the body has, as far as that is known.
	Length int64 `json:"length"`
	// Parser is what read the body, when something recognized it.
	Parser *Recognition `json:"parser,omitempty"`
	// Metadata is what the parser found worth knowing, as a JSON object:
	// the refs a push changes, the fields a form sends. It is redacted, like
	// everything else shown, and is present from the first ask.
	Metadata json.RawMessage `json:"metadata,omitempty"`
	// ParseError says, in a sentence, why the parser named could not read a
	// body that claims to be what it reads. A body that is not what it says
	// is evidence, and is shown as such rather than described as something
	// weaker (ADR 26-09-26-240 §1).
	ParseError string `json:"parseError,omitempty"`
	// Content is the body, redacted and rendered the way its parser renders
	// it. It is absent until the judge asks to be shown the body, and then
	// holds as much of it as the budget allowed, which may be none of it.
	Content *string `json:"content,omitempty"`
	// Missing says, in a sentence for the judge, why Content is not the whole
	// body: larger than may be shown, not text, an encoding that could not be
	// decoded. It is said rather than hidden, so the judge decides knowing
	// what it is not being shown.
	//
	// It is an answer to having been asked, never part of the first
	// description: with Content set when some of the body was shown, or none
	// of it, and with Content absent when nothing could be shown at all.
	Missing string `json:"missing,omitempty"`
}

// Supplied reports whether the judge has been answered about the body's own
// bytes: shown some or all of them, or told why none can be.
func (b *Body) Supplied() bool { return b != nil && (b.Content != nil || b.Missing != "") }

// OperationInBody says why a request's operation is in its body, or nothing
// when it is not known to be (ADR 26-09-26-240 §2). An allow for such a
// request was about that body, and letting it stand for the route would allow
// every other body sent there (Job.Admits).
//
// It goes by what the request was recognized as, not by what its metadata
// happened to say: a push is its ref updates whether or not they were read,
// and an endpoint that reads its body does so for every request to it,
// including one that happened to send none. An endpoint is taken to read its
// body unless this package names it as one whose method and path are its
// whole operation (operationOutsideBody). It is asked of the request an
// allow is granted on and of every request a standing allow would cover. A
// parser's generic description of a body — a JSON object's keys — is not
// the operation, and does not stop a route standing; posting review comments
// one call at a time is what standing allows are for (ADR 26-09-25-428).
func (r *Request) OperationInBody() string {
	switch {
	case r == nil:
		return ""
	case r.Protocol != nil:
		return "a request in a protocol Discobox recognized carries its operation in its body, and an allow for one does not stand"
	case r.Endpoint != nil && !operationOutsideBody[r.Endpoint.Name]:
		return "an endpoint whose operation Discobox reads from its body was allowed for that body, and the allow does not stand"
	case r.Body != nil && r.Body.ParseError != "":
		return "a body its parser could not read may be anything, and an allow for it does not stand"
	default:
		return ""
	}
}

// Answers reports whether asking for the body again would change nothing,
// which is a judge that has spent a round and decided nothing.
//
// Nothing more can be shown when none of it could be shown at all, when all
// of it arrived, when none of it was shown and none would be again, or when
// what arrived is already everything this ask allows. A larger budget can
// still show more.
func (b *Body) Answers(need Need) bool {
	if b == nil || !b.Supplied() {
		return false
	}
	switch {
	case b.Content == nil:
		// Asked, and nothing could be shown.
		return true
	case b.Missing == "":
		// All of it arrived, whatever budget the ask names.
		return true
	case *b.Content == "":
		// None of it was shown, and none would be.
		return true
	default:
		return len(*b.Content) >= need.Budget()
	}
}

// Validate refuses a job that could not be judged, before a model is paid to
// read it. Everything it checks is the trusted side's own doing: a caller
// supplies evidence, and the shape of a job is not a caller's to get wrong.
func (j Job) Validate() error {
	if strings.TrimSpace(j.Purpose) == "" || strings.TrimSpace(j.Host) == "" {
		return errors.New("a job is judged against an approved use and its host")
	}
	if j.Round < 1 || j.Round > MaxRounds {
		return fmt.Errorf("round %d is outside the %d a job may be asked", j.Round, MaxRounds)
	}
	switch j.Kind {
	case KindCommand:
		if j.Request != nil {
			return errors.New("a command job carries no request")
		}
		if len(j.Command) == 0 || strings.TrimSpace(j.Command[0]) == "" {
			return errors.New("a command job requires the argv it is about to run")
		}
		if j.Round != 1 {
			return errors.New("a command job is asked once: there is nothing further to show")
		}
		if j.Stdin != nil && len(j.Stdin.Content) > MaxBodyBytes {
			return errors.New("the input shown exceeds what a judge may be shown")
		}
		if j.Stdin != nil && j.Stdin.Content == "" && strings.TrimSpace(j.Stdin.Missing) == "" {
			return errors.New("an input with nothing shown says why")
		}
		if err := j.Reported.validate(); err != nil {
			return err
		}
	case KindRequest:
		if j.Request == nil {
			return errors.New("a request job requires the request observed")
		}
		if strings.TrimSpace(j.Request.Method) == "" || strings.TrimSpace(j.Request.URL) == "" {
			return errors.New("a request job requires the method and destination observed")
		}
		if !j.Request.Protocol.valid() || !j.Request.Endpoint.valid() {
			return errors.New("what a request was recognized as is named, lower case, and versioned")
		}
		if err := j.Request.Body.validate(); err != nil {
			return err
		}
		// The first ask describes the body and does not carry it, and says
		// nothing about what could not be shown: what is missing is an answer
		// to having been asked, and a body described as unshowable before
		// anyone asked reads as one nothing could ever show — which is how a
		// large body would be refused on its size rather than read.
		if j.Round == 1 && j.Request.Body.Supplied() {
			return errors.New("the first ask describes the body rather than showing it, or saying what of it cannot be shown")
		}
		if j.Round > 1 && !j.Request.Body.Supplied() {
			return errors.New("a later round answers what the judge asked to be shown")
		}
	case KindDelegation:
		if j.Request != nil || len(j.Command) > 0 {
			return errors.New("a delegation job carries neither a command nor a request")
		}
		if len(j.Uses) == 0 {
			return errors.New("a delegation job requires the uses about to be handed on")
		}
		for _, use := range j.Uses {
			if strings.TrimSpace(use) == "" {
				return errors.New("a use about to be handed on says what it is for")
			}
		}
		if j.Round != 1 {
			return errors.New("a delegation job is asked once: there is nothing further to show")
		}
	case KindRule:
		if j.Request != nil || len(j.Command) > 0 {
			return errors.New("a rule job carries neither a command nor a request")
		}
		if len(j.Uses) == 0 {
			return errors.New("a rule job requires the uses asked for")
		}
		for _, use := range j.Uses {
			if strings.TrimSpace(use) == "" {
				return errors.New("a use asked for says what it is for")
			}
		}
		if j.Round != 1 {
			return errors.New("a rule job is asked once: there is nothing further to show")
		}
	default:
		return fmt.Errorf("unknown judge job kind %q", j.Kind)
	}
	if j.Kind != KindCommand && (j.Stdin != nil || j.Reported != nil) {
		return errors.New("only a command job reads standard input or runs somewhere")
	}
	if len(j.Facts) > MaxFacts {
		return fmt.Errorf("a job carries at most %d facts", MaxFacts)
	}
	for _, fact := range j.Facts {
		if strings.TrimSpace(fact) == "" {
			return errors.New("a fact says something")
		}
	}
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if len(data) > MaxInput {
		return errors.New("the evidence for one job exceeds what a judge may be shown")
	}
	return nil
}

func (r *Reported) validate() error {
	if r == nil {
		return nil
	}
	for _, field := range []string{r.WorkingDirectory, r.RepositoryRoot, r.RefCommit, r.RefSubject} {
		if len(field) > MaxReportedBytes {
			return errors.New("what a discobox reports about where a command runs is a path or a line")
		}
	}
	return nil
}

func (b *Body) validate() error {
	if b == nil {
		return nil
	}
	if !b.Parser.valid() {
		return errors.New("a body's parser is named, lower case, and versioned")
	}
	if b.Parser == nil && (len(b.Metadata) > 0 || b.ParseError != "") {
		return errors.New("only a parser says what is in a body")
	}
	if len(b.Metadata) > 0 {
		if len(b.Metadata) > MaxMetadataBytes {
			return errors.New("a body's metadata exceeds what a parser may say about it")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(b.Metadata, &fields); err != nil || fields == nil {
			return errors.New("a body's metadata is one JSON object")
		}
	}
	if b.Content != nil && len(*b.Content) > MaxBodyBytes {
		return errors.New("the body shown exceeds what a judge may be shown")
	}
	return nil
}

// CanShowBody reports whether asking to be shown this job's body could still
// show something: a request job, before its last round, with a body there is
// something of that has not already been shown in full. Prompt says so beside
// the request; a judge that decides in code rather than in a model asks it
// directly.
func (j Job) CanShowBody() bool {
	if j.Kind != KindRequest || j.Request == nil || j.Round >= MaxRounds {
		return false
	}
	body := j.Request.Body
	return body != nil && body.Length != 0 && !body.Answers(Need{Body: true, Bytes: MaxBodyBytes})
}

// Prompt is the job as the judge receives it: JSON, so that the boundary
// between what Discobox says and what the request says is a structure rather
// than a sentence an injected instruction can imitate.
func Prompt(j Job) (string, error) {
	if err := j.Validate(); err != nil {
		return "", err
	}
	j.BodyCanBeShown, j.AsksLeft = false, 0
	if j.CanShowBody() {
		j.BodyCanBeShown, j.AsksLeft = true, MaxRounds-j.Round
	}
	data, err := json.Marshal(j)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Digest is the SHA-256 of the job as the judge receives it, in hex: what a
// question already answered is found by. Two jobs with one digest are one
// question, and a job any word of which changed is another.
func Digest(j Job) (string, error) {
	prompt, err := Prompt(j)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:]), nil
}
