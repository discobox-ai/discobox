// Package judge is the question Discobox puts to a model before a credential
// is used, and the answer it will accept back (ADR 0141).
//
// It holds the contract and nothing that runs it: the job a caller may ask,
// the system prompt and schema the trusted side supplies, and the strict
// decoding of what comes back. A caller never chooses the prompt, the model,
// the tools, or the schema — those are here, so that what is asked of the
// judge is the same wherever the asking happens.
package judge

import (
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
	PromptVersion = "1"
	// Timeout bounds one exchange — every round of it together, not each ask.
	// A request is being held open while the judge thinks.
	//
	// The caller owns it, because only the caller knows an exchange is under
	// way: it passes a context carrying the remaining time, and each ask is
	// bounded by whichever is sooner, that or this. A judge runtime applies it
	// on its own as a ceiling for a caller that passed no deadline at all,
	// which bounds one ask rather than the exchange.
	Timeout = 90 * time.Second
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
)

// The kinds of job. Each is judged against one approved use.
const (
	// KindCommand is a command a discobox is about to run under an approved
	// use, judged before any credential is minted for it.
	KindCommand = "command"
	// KindRequest is a request observed by the proxy, judged before the
	// credentials it carries are resolved.
	KindRequest = "request"
)

// How a body is written when the judge is shown one.
const (
	// FormText is the body as sent, as text.
	FormText = "text"
	// FormJSON is the body parsed and written back as JSON, which is how a
	// request whose operation lives in a JSON document reads clearly.
	FormJSON = "json"
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
	// Command is the argv, for a command job. For a request job it is what
	// the discobox declared it was running, which is context and not
	// authority: a declaration cannot override the request observed.
	Command []string `json:"command,omitempty"`
	// Request is the request observed by the proxy, for a request job.
	Request *Request `json:"request,omitempty"`
}

// Request is a request as the proxy saw it, before any credential was
// substituted into it and with every credential-bearing value redacted.
type Request struct {
	Method string `json:"method"`
	// URL is the destination authority and port, path, and query.
	URL string `json:"url"`
	// Headers are the headers worth weighing, redacted.
	Headers map[string][]string `json:"headers,omitempty"`
	// Body describes the request's body, and carries it once the judge has
	// asked for it. A request with no body has none.
	Body *Body `json:"body,omitempty"`
}

// Body is what the judge is told about a request's body.
//
// The first ask describes it and does not carry it: most requests are decided
// by what they are and where they go, and a body sent every time is tokens
// spent answering a question the URL has already settled. The judge asks to be
// shown it when the operation lives in there (ADR 0141 §6).
type Body struct {
	// MediaType is the content type as declared, if it was.
	MediaType string `json:"mediaType,omitempty"`
	// Length is how many bytes the body has, as far as that is known.
	Length int64 `json:"length"`
	// Form is how Content is written, and is empty until the judge has asked
	// to be shown the body.
	Form string `json:"form,omitempty"`
	// Content is the body, in Form, redacted. It is present only once asked
	// for, and only as much of it as the budget allowed.
	Content string `json:"content,omitempty"`
	// Missing says, in a sentence for the judge, why Content is not the whole
	// body: larger than may be shown, not text, an encoding that could not be
	// decoded. It is said rather than hidden, so the judge decides knowing
	// what it is not being shown (ADR 0141 §6).
	//
	// It is an answer to having been asked, never part of the first
	// description: with Form set when that form showed some of the body, or
	// none of it, and with Form empty when nothing could be shown at all.
	Missing string `json:"missing,omitempty"`
}

// Supplied reports whether the body has been shown to the judge at all.
func (b *Body) Supplied() bool { return b != nil && b.Form != "" }

// Answers reports whether asking for this again would change nothing, which
// is a judge that has spent a round and decided nothing.
//
// Nothing more can be shown when the body could not be shown at all, when the
// form asked for is the one already shown and all of it arrived, when that
// form showed none of it, or when what arrived is already everything this ask
// allows. A larger budget, or the other form, can still show more.
func (b *Body) Answers(need Need) bool {
	if b == nil {
		return false
	}
	if !b.Supplied() {
		// Described and not shown is the ordinary first ask. Described, with
		// why it is not shown, is a body nothing can show in any form.
		return b.Missing != ""
	}
	if b.Form != need.Body {
		return false
	}
	switch {
	case b.Missing == "":
		// All of it arrived, whatever budget the ask names.
		return true
	case b.Content == "":
		// This form showed none of it, and would again.
		return true
	default:
		return len(b.Content) >= need.Budget()
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
	case KindRequest:
		if j.Request == nil {
			return errors.New("a request job requires the request observed")
		}
		if strings.TrimSpace(j.Request.Method) == "" || strings.TrimSpace(j.Request.URL) == "" {
			return errors.New("a request job requires the method and destination observed")
		}
		if err := j.Request.Body.validate(); err != nil {
			return err
		}
		// The first ask describes the body and does not carry it, and says
		// nothing about what could not be shown: what is missing is an answer
		// to having been asked, and a body described as unshowable before
		// anyone asked reads as one nothing could ever show — which is how a
		// large body would be refused on its size rather than read.
		if j.Round == 1 && (j.Request.Body.Supplied() || j.Request.Body.missing() != "") {
			return errors.New("the first ask describes the body rather than showing it, or saying what of it cannot be shown")
		}
		if j.Round > 1 && !j.Request.Body.Supplied() && j.Request.Body.missing() == "" {
			return errors.New("a later round answers what the judge asked to be shown")
		}
	default:
		return fmt.Errorf("unknown judge job kind %q", j.Kind)
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

func (b *Body) validate() error {
	if b == nil {
		return nil
	}
	switch b.Form {
	case "", FormText, FormJSON:
	default:
		return fmt.Errorf("a body is shown as %s or %s, not %q", FormText, FormJSON, b.Form)
	}
	if b.Form == "" && b.Content != "" {
		return errors.New("a body carrying content says which form it is in")
	}
	if len(b.Content) > MaxBodyBytes {
		return errors.New("the body shown exceeds what a judge may be shown")
	}
	return nil
}

func (b *Body) missing() string {
	if b == nil {
		return ""
	}
	return b.Missing
}

// Prompt is the job as the judge receives it: JSON, so that the boundary
// between what Discobox says and what the request says is a structure rather
// than a sentence an injected instruction can imitate.
func Prompt(j Job) (string, error) {
	if err := j.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(j)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
