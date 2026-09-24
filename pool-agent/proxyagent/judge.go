package proxyagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/pool-agent/wire"
	"github.com/discobox-ai/discobox/proxy"
)

// Asking the project's judge (ADR 0150 §4).
//
// A request carrying a credential is authorized before any of it is resolved,
// and what authorizes it is a model reading the request against the sentence
// somebody approved. The pool does not hold that sentence and does not send
// one: it names the discobox and the use, and the control plane reads what the
// use allows from the live grant.

// judgeAsk is what this pool puts to the control plane. It is written by hand
// rather than taken from the generated client because the pool agent talks to
// the control plane over its own small surface (see credentials.go).
type judgeAsk struct {
	SandboxID string         `json:"sandboxId"`
	UseID     string         `json:"useId"`
	Round     int            `json:"round"`
	Request   *judgeEvidence `json:"request"`
}

// judgeEvidence is the request as the proxy saw it, redacted.
type judgeEvidence struct {
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    *judgeEvidenceBody  `json:"body,omitempty"`
}

// judgeEvidenceBody describes a body without carrying it. The first ask says
// what there is; the judge asks to be shown it if the decision lives in there.
type judgeEvidenceBody struct {
	MediaType string `json:"mediaType,omitempty"`
	Length    int64  `json:"length"`
}

// judgeAnswer is what came back.
type judgeAnswer struct {
	Allow  *bool  `json:"allow,omitempty"`
	Reason string `json:"reason"`
	Need   *struct {
		Body  string `json:"body"`
		Bytes int64  `json:"bytes,omitempty"`
	} `json:"need,omitempty"`
}

// judgingDisabledKind is how a server that does not judge says so. A pool has
// to tell that apart from a judge that failed: one means stop asking, the
// other means no credential goes out.
const judgingDisabledKind = "urn:discobox:problem:judging-disabled"

// judgingDisabledFor is how long a pool takes that answer for. It is short
// because it is the wrong side to be wrong on for long: a server that is
// turned on should start being asked without anybody restarting a pool.
const judgingDisabledFor = 5 * time.Minute

// judgingUnansweredFor is how long a pool waits after an ask it could not get
// an answer to at all, before it has ever been told this server judges. It is
// shorter, because being wrong this way means not judging something that
// should have been.
const judgingUnansweredFor = 30 * time.Second

// outcome is what came back, in the only terms that matter here: what it says
// about this request, and what it says about whether this server judges at
// all. The two are not the same question, and an earlier version of this code
// answered the second with the first — which let a discobox turn judging off
// for a whole pool by making its own ask unanswerable.
//
// What carries the claim "this server judges" is a verdict and nothing else.
// Every other status a pool sees is as likely to come from something between
// it and the handler — a load balancer restarting, a body limit, an expired
// assertion — as from the judge, and treating those as proof would arm a
// refusal on a server that never turned judging on.
type outcome int

const (
	// outcomeUnknown is nobody having answered: a control plane that could not
	// be reached, or that failed in a way of its own. Whether it refuses is
	// the one thing that depends on what this pool has learned.
	//
	// It is the zero value on purpose. An outcome nobody set is one nothing
	// has established, and the safe reading of that is the cautious one.
	outcomeUnknown outcome = iota
	// outcomeNobodyJudges is the server saying so, or having no such route to
	// say it with. Neither is a refusal, and both are worth remembering for a
	// while.
	outcomeNobodyJudges
	// outcomeRefused is the control plane declining this ask on its own terms:
	// evidence it would not take, a use it would not name. It refuses this
	// request and says nothing about the server, so it neither latches nor
	// silences the next one — a request a discobox shaped to be refused must
	// cost that discobox its request and nothing else.
	outcomeRefused
)

// A verdict is not an outcome here: it arrives as a nil error with an answer
// beside it, and it is the only thing that establishes that this server
// judges.

// asked is an ask that did not produce a verdict, and what it turned out to be.
type asked struct {
	kind outcome
	said string
}

func (e *asked) Error() string { return e.said }

// outcomeOf reads what an error means for judging.
func outcomeOf(err error) outcome {
	var result *asked
	if errors.As(err, &result) {
		return result.kind
	}
	return outcomeUnknown
}

// judgeHTTPTimeout is what a pool waits for a verdict. It is deliberately
// longer than the bound the control plane puts on the same exchange
// (judge.Timeout plus its own routing grace), so that a judge which takes its
// time is answered by the deadline nearest it rather than cut off here — where
// the answer would be indistinguishable from a control plane that never
// replied.
const judgeHTTPTimeout = judge.Timeout + 45*time.Second

// judgeClient asks the control plane for a verdict, and remembers what it has
// learned about whether there is anybody to ask.
//
// The thing it has to keep straight is the difference between a server that
// does not judge and a server that does and could not be reached. Both look
// like "no answer" at the moment of asking, and they call for opposite
// behavior: the first must not break a discobox that was working before this
// feature existed, and the second must not let a credential out.
//
// So a failure is only a refusal once this pool has had an answer from a
// server that judges. Until then it has no reason to believe judging is on at
// all, and a control plane it cannot reach is not evidence that it is.
type judgeClient struct {
	plane *controlPlaneCredentials

	mu sync.Mutex
	// enforcing is set by the first answer this pool gets from a server that
	// judges, and never unset: a server does not stop judging because one ask
	// failed, and treating a failure as "probably turned off" is how a
	// credential leaves on a request nothing agreed to.
	enforcing bool
	// quietUntil is when to try again, after being told there is nobody to ask
	// or after an ask that got no answer at all.
	quietUntil time.Time
}

// asking reports whether this pool should put a question at all.
func (c *judgeClient) asking(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !now.Before(c.quietUntil)
}

// answered records that a server which judges answered. From here on, an ask
// that cannot be answered is a request that goes nowhere.
func (c *judgeClient) answered() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enforcing = true
}

// disabled records that the control plane said there is nobody to ask.
func (c *judgeClient) disabled(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.quietUntil = now.Add(judgingDisabledFor)
}

// unanswered records an ask that got no answer, and reports whether that
// refuses the request. It does only where this pool already knows the server
// judges; otherwise the request is allowed and the next one is not asked about
// for a moment, so an opted-out server with a flaky link is not a discobox
// that has stopped working.
func (c *judgeClient) unanswered(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.enforcing {
		return true
	}
	c.quietUntil = now.Add(judgingUnansweredFor)
	return false
}

// ask puts one request to the judge of the project that owns this pool.
func (c *judgeClient) ask(ctx context.Context, ask judgeAsk) (judgeAnswer, error) {
	rc, err := readResolveContext(c.plane.contextPath)
	if err != nil || rc.Token == "" || rc.ControlPlaneURL == "" || rc.PoolID == "" {
		return judgeAnswer{}, errors.New("this pool cannot reach the control plane yet")
	}
	payload, err := json.Marshal(ask)
	if err != nil {
		return judgeAnswer{}, err
	}
	endpoint := fmt.Sprintf("%s/api/pools/%s/judge", rc.ControlPlaneURL, url.PathEscape(rc.PoolID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return judgeAnswer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rc.Token)
	resp, err := c.plane.client.Do(req)
	if err != nil {
		return judgeAnswer{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return judgeAnswer{}, judgeRefusal(resp)
	}
	var answer judgeAnswer
	if err := json.NewDecoder(io.LimitReader(resp.Body, int64(judge.MaxOutput))).Decode(&answer); err != nil {
		return judgeAnswer{}, fmt.Errorf("the judge's answer could not be read: %w", err)
	}
	return answer, nil
}

// judgeRefusal reads a problem document, keeping the sentence for whoever
// asked and sorting the status into what it means for judging.
func judgeRefusal(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var problem struct {
		Detail string `json:"detail"`
		Title  string `json:"title"`
		Type   string `json:"type"`
	}
	_ = json.Unmarshal(data, &problem)
	message := strings.TrimSpace(problem.Detail)
	if message == "" {
		message = strings.TrimSpace(problem.Title)
	}
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	// Whether the judging handler answered at all, as opposed to something
	// between this pool and it. Only what this server wrote says anything
	// about this server: an ingress returning 429, a gateway returning 502
	// while the control plane restarts, a proxy's plain-text 404 — each is a
	// status with no opinion about judging, and reading one as an opinion is
	// how both of this function's earlier versions were wrong.
	ours := strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") &&
		(problem.Type != "" || problem.Detail != "" || problem.Title != "")
	switch {
	case ours && problem.Type == judgingDisabledKind:
		// The one answer that says judging is off, and it counts only from
		// this server. Nothing in the path can put a pool in this branch by
		// accident, and the branch it would reach is the one with the worst
		// outcome — so it is held to the same test as the rest.
		return &asked{kind: outcomeNobodyJudges, said: message}
	case ours && resp.StatusCode >= 400 && resp.StatusCode < 500:
		// This server declining this ask: evidence it would not take, a pool
		// or project it could not find, a token it would not accept. It
		// refuses the request and says nothing more, so a discobox that shapes
		// a request to be refused costs itself and nobody else.
		return &asked{kind: outcomeRefused, said: message}
	default:
		// Anything else, including a 404 from something that is not the
		// handler. Nobody has said whether this server judges, which is what
		// outcomeUnknown means.
		return &asked{kind: outcomeUnknown, said: message}
	}
}

// evidenceOf is the request as the judge is shown it: what identifies the
// operation, with everything that could carry a credential taken out.
//
// The body is described and not carried (ADR 0150 §6). What can be said about
// it without reading it is what the request declared, and only a request that
// declared a length is described at all: the contract says a body's length in
// bytes, with no way to spell "some unknown number of them", so a chunked
// upload described here would be a body reported as empty. Saying nothing is
// the honest form of not knowing. Reading the body — which is what would
// actually answer this — is the round this does not run yet.
func evidenceOf(req proxy.SecretAuthorizeRequest) *judgeEvidence {
	evidence := &judgeEvidence{
		Method:  req.Method,
		URL:     redactedURL(req.URL, req.Sentinels),
		Headers: redactHeaders(req.Header, req.Sentinels),
	}
	if length, err := strconv.ParseInt(strings.TrimSpace(req.Header.Get("Content-Length")), 10, 64); err == nil && length > 0 {
		evidence.Body = &judgeEvidenceBody{
			MediaType: strings.TrimSpace(req.Header.Get("Content-Type")),
			Length:    length,
		}
	}
	return evidence
}

// shownHeaders are the headers a judge is shown the value of. Everything else
// keeps its name and loses its value.
//
// It is an allowlist because the alternative cannot be got right: a list of
// the headers that carry credentials is a list of the ones somebody thought
// of, and every API invents another (`Private-Token`, `X-Goog-Api-Key`,
// `X-Functions-Key`). ADR 0150 §6 asks for the headers worth weighing, and
// these are the ones that say what an operation is rather than who is making
// it. A header not here is still reported, by name, so the judge knows it was
// sent.
var shownHeaders = map[string]bool{
	"accept":               true,
	"accept-encoding":      true,
	"accept-language":      true,
	"anthropic-version":    true,
	"content-encoding":     true,
	"content-length":       true,
	"content-type":         true,
	"if-match":             true,
	"if-none-match":        true,
	"openai-beta":          true,
	"origin":               true,
	"referer":              true,
	"user-agent":           true,
	"x-github-api-version": true,
}

// credentialQueryParams are query parameters whose values are never shown. The
// query string is evidence — it is half of what identifies an operation — so
// it cannot be dropped wholesale the way a header's value can, and this is the
// denylist that the header side deliberately avoids. It is the common spellings
// and it is not exhaustive; what makes that survivable is that a sentinel is
// redacted wherever it appears, so what leaks here is a credential a discobox
// brought itself.
var credentialQueryParams = map[string]bool{
	"access_token":         true,
	"api_key":              true,
	"apikey":               true,
	"auth":                 true,
	"code":                 true,
	"key":                  true,
	"password":             true,
	"secret":               true,
	"sig":                  true,
	"signature":            true,
	"token":                true,
	"x-amz-credential":     true,
	"x-amz-security-token": true,
	"x-amz-signature":      true,
}

// redactedURL is the destination as the judge is shown it: the same URL with
// every sentinel taken out and the value of anything that reads like a
// credential replaced.
//
// It rewrites the query in place rather than parsing and re-encoding it.
// url.Values drops what it cannot parse — a segment containing a semicolon, a
// bad percent-escape — and re-encoding sorts what survives, so a discobox
// could have hidden the operative parameter of a request from the judge while
// still sending it upstream, simply by adding a parameter that made this
// rewrite happen. What the judge is shown has to be what was sent.
func redactedURL(raw string, sentinels []string) string {
	redacted := redactSentinels(raw, sentinels)
	mark := strings.IndexByte(redacted, '?')
	if mark < 0 || mark == len(redacted)-1 {
		return redacted
	}
	head, query := redacted[:mark+1], redacted[mark+1:]
	fragment := ""
	if hash := strings.IndexByte(query, '#'); hash >= 0 {
		query, fragment = query[:hash], query[hash:]
	}
	segments := strings.Split(query, "&")
	for i, segment := range segments {
		equals := strings.IndexByte(segment, '=')
		if equals < 0 {
			continue
		}
		name, err := url.QueryUnescape(segment[:equals])
		if err != nil {
			// Unreadable as a name, so it is not one this knows to redact. It
			// is left exactly as it arrived rather than dropped.
			continue
		}
		if credentialQueryParams[strings.ToLower(name)] {
			segments[i] = segment[:equals+1] + url.QueryEscape(redactedValue)
		}
	}
	return head + strings.Join(segments, "&") + fragment
}

// redactedValue is what stands in for something a judge may not see. It keeps
// the fact that the header was there, which is worth knowing, and drops what
// it carried.
const redactedValue = "<redacted>"

// redactHeaders is the headers as the judge is shown them.
func redactHeaders(header http.Header, sentinels []string) map[string][]string {
	if len(header) == 0 {
		return nil
	}
	out := make(map[string][]string, len(header))
	for name, values := range header {
		if !shownHeaders[strings.ToLower(name)] {
			out[name] = []string{redactedValue}
			continue
		}
		shown := make([]string, 0, len(values))
		for _, value := range values {
			shown = append(shown, redactSentinels(value, sentinels))
		}
		out[name] = shown
	}
	return out
}

// redactSentinels takes this request's sentinels out of a value. A sentinel is
// not a credential, but it is the string that stands for one, and a model has
// no use for it.
//
// It goes through the proxy's own scan rather than a string replace of its
// own: a sentinel travels base64-encoded as often as it travels literally
// (Authorization: Basic), and the matcher that found it is the only thing that
// knows where it was.
func redactSentinels(value string, sentinels []string) string {
	return proxy.RedactSentinels(value, sentinels, redactedValue)
}

// judgeHTTPClient reaches the control plane the way the resolver does, with a
// verdict's patience rather than a lookup's.
func judgeHTTPClient() *http.Client {
	client := &http.Client{Timeout: judgeHTTPTimeout}
	if url := strings.TrimSpace(os.Getenv(envControlPlaneURL)); url != "" {
		if _, resolved, err := wire.HTTPClient(url, judgeHTTPTimeout); err == nil {
			client = resolved
		}
	}
	return client
}
