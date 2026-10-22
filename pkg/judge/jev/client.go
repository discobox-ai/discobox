// Package jev puts a judge.Job to TypeSafe's Jev and turns what it says into a
// judge.Answer (ADR 26-10-01-324).
//
// Jev answers typed questions about a state with calibrated probabilities and
// writes no text. So the question is this package's, built from the job with
// the authorization in each question's instructions and only the evidence in
// the state, and the verdict is decided here, in code, from the probabilities:
// a reason, a refusal, and an ask to be shown the body are all this side's.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is TypeSafe's API.
	DefaultBaseURL = "https://api.typesafe.ai"
	// DefaultModel is the Jev the thresholds were tuned against. It is a
	// version, not the jev-latest alias: an alias moves when TypeSafe ships,
	// and the probabilities a threshold was set against move with it.
	DefaultModel = "jev-1.13.0"

	// maxResponse bounds what Jev's answer may be. A handful of questions
	// answered is a few hundred bytes.
	maxResponse = 1 << 20
	// maxAttempts is how many times one ask is sent while Jev says it is busy.
	maxAttempts = 4
	// firstBackoff doubles on each retry unless Jev says how long to wait.
	firstBackoff = 250 * time.Millisecond
	// maxBackoff caps any one wait, including one Jev asked for.
	maxBackoff = 5 * time.Second
)

// Config is what a Client needs. Only the key is required.
type Config struct {
	APIKey string
	// Model defaults to DefaultModel.
	Model string
	// BaseURL defaults to DefaultBaseURL.
	BaseURL string
	// HTTPClient defaults to a plain client. Every call is bounded by the
	// context it is given, not by the client.
	HTTPClient *http.Client
}

// Client asks Jev.
type Client struct {
	apiKey  string
	model   string
	baseURL string
	http    *http.Client
}

// New is a Client for config. A Client with no key would have every ask
// refused, so it is refused here instead.
func New(config Config) (*Client, error) {
	key := strings.TrimSpace(config.APIKey)
	if key == "" {
		return nil, errors.New("a Jev API key is required")
	}
	client := &Client{
		apiKey:  key,
		model:   strings.TrimSpace(config.Model),
		baseURL: strings.TrimRight(strings.TrimSpace(config.BaseURL), "/"),
		http:    config.HTTPClient,
	}
	if client.model == "" {
		client.model = DefaultModel
	}
	if client.baseURL == "" {
		client.baseURL = DefaultBaseURL
	}
	if client.http == nil {
		client.http = &http.Client{}
	}
	return client, nil
}

// Model is the model this client asks for.
func (c *Client) Model() string { return c.model }

// StatusError is Jev answering with something other than an answer.
type StatusError struct {
	Status int
	// Message is what Jev said, trimmed. It may name the request's fields and
	// is for an operator's log, never for the discobox that asked.
	Message string
	// retryAfter is how long Jev asked to be left alone, when it said.
	retryAfter time.Duration
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("jev answered HTTP %d", e.Status)
	}
	return fmt.Sprintf("jev answered HTTP %d: %s", e.Status, e.Message)
}

// Busy reports whether Jev refused for load: its rate limit, or overloaded.
func (e *StatusError) Busy() bool {
	return e.Status == http.StatusTooManyRequests || e.Status == statusOverloaded
}

// Unauthorized reports whether Jev refused the key.
func (e *StatusError) Unauthorized() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

// statusOverloaded is TypeSafe's own status for a service under load.
const statusOverloaded = 529

// request is the body of POST /v1/systemone.
type request struct {
	State     any                 `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]question `json:"questions"`
}

// question is a noul: the only kind this package asks, because every
// judgment it puts is a yes or a no and each is decided against a threshold.
type question struct {
	Type         string    `json:"type"`
	Instructions any       `json:"instructions"`
	Criteria     *criteria `json:"criteria,omitempty"`
}

// criteria says what a yes and a no mean.
type criteria struct {
	True  string `json:"true"`
	False string `json:"false"`
}

// response is Jev's answer: one per question, under the IDs asked.
type response struct {
	Model   string            `json:"model"`
	Answers map[string]answer `json:"answers"`
}

type answer struct {
	Type string   `json:"type"`
	Noul *float64 `json:"noul"`
}

// input is the body of the request that asks Jev questions about state: what
// Jev is sent, byte for byte, and what a verdict records it was sent.
func (c *Client) input(state any, questions map[string]question) (json.RawMessage, error) {
	return json.Marshal(request{State: state, Model: c.model, Questions: questions})
}

// ask sends input, the body c.input built, and returns the probability of yes
// for each of questions, and the versioned model that answered. Every
// question asked must come back as a noul between 0 and 1; anything else is
// no answer.
func (c *Client) ask(ctx context.Context, input json.RawMessage, questions map[string]question) (map[string]float64, string, error) {
	var answered response
	if err := c.post(ctx, input, &answered); err != nil {
		return nil, "", err
	}
	probabilities := make(map[string]float64, len(questions))
	for id := range questions {
		got, ok := answered.Answers[id]
		if !ok {
			return nil, "", fmt.Errorf("jev did not answer %q", id)
		}
		if got.Type != "noul" || got.Noul == nil {
			return nil, "", fmt.Errorf("jev answered %q with a %q, not a noul", id, got.Type)
		}
		p := *got.Noul
		// NaN fails both comparisons and is caught here too.
		if !(p >= 0 && p <= 1) {
			return nil, "", fmt.Errorf("jev answered %q with %v, which is not a probability", id, p)
		}
		probabilities[id] = p
	}
	model := strings.TrimSpace(answered.Model)
	if model == "" {
		model = c.model
	}
	return probabilities, model, nil
}

// post sends body to the evaluation endpoint, retrying while Jev says it is
// busy and the context leaves time to wait, and decodes the answer into out.
func (c *Client) post(ctx context.Context, body []byte, out any) error {
	wait := firstBackoff
	for attempt := 1; ; attempt++ {
		err := c.postOnce(ctx, body, out)
		var status *StatusError
		retryable := errors.As(err, &status) && (status.Busy() || status.Status >= 500)
		if err == nil || !retryable || attempt == maxAttempts {
			return err
		}
		delay := wait
		if status.retryAfter > 0 {
			delay = status.retryAfter
		}
		delay = min(delay, maxBackoff)
		// A wait that would outlast the ask is not worth starting: the busy
		// answer is the one to give back.
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		wait *= 2
	}
}

func (c *Client) postOnce(ctx context.Context, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return statusError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(out); err != nil {
		return fmt.Errorf("jev answered with something unreadable: %w", err)
	}
	return nil
}

// statusError reads a refusal: its status, a trimmed sentence of what it said,
// and how long it asked to be left alone.
func statusError(resp *http.Response) *StatusError {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	message := strings.TrimSpace(string(data))
	var said struct {
		Detail  any    `json:"detail"`
		Error   any    `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &said) == nil {
		for _, field := range []any{said.Message, said.Error, said.Detail} {
			if text, ok := field.(string); ok && strings.TrimSpace(text) != "" {
				message = strings.TrimSpace(text)
				break
			}
		}
	}
	if len(message) > 300 {
		message = message[:300] + "…"
	}
	failure := &StatusError{Status: resp.StatusCode, Message: message}
	if seconds, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && seconds > 0 {
		failure.retryAfter = time.Duration(seconds) * time.Second
	}
	return failure
}
