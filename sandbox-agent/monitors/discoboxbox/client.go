package discoboxbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/pkg/sse"
)

// boxEvent is what this monitor reads of a sandbox event's data,
// `SandboxEventData` in server.yaml (ADR 26-10-29-316 §1, ADR 26-11-01-560
// §2): the box's whole state. A local struct, not `api/gen`'s type: runtime
// code does not use the public API's DTOs (the root REVIEW.md), and a field a
// newer server adds breaks nothing.
type boxEvent struct {
	SandboxID    string    `json:"sandboxId"`
	Name         string    `json:"name"`
	At           time.Time `json:"at"`
	DesiredState string    `json:"desiredState"`
	State        string    `json:"state"`
	RuntimeState string    `json:"runtimeState"`
	DisplayState string    `json:"displayState"`
	Program      *struct {
		State string `json:"state"`
		Kind  string `json:"kind"`
	} `json:"program"`
	Tags            map[string]string `json:"tags"`
	ParentSandboxID string            `json:"parentSandboxId"`
}

// The stream's event types.
const (
	eventCreated = "sandbox.created"
	eventUpdated = "sandbox.updated"
	eventDeleted = "sandbox.deleted"
	eventReset   = "reset"
)

// name is what an event calls the box: its configured name, the one the
// stream carries, else its ID. The display name follows its program's title,
// which the stream leaves out (ADR 26-10-29-316 §1).
func (e boxEvent) name() string {
	if name := strings.TrimSpace(e.Name); name != "" {
		return name
	}
	return e.SandboxID
}

// secretRequest is what this monitor reads of a SecretRequest.
type secretRequest struct {
	ID            string    `json:"id"`
	SandboxID     string    `json:"sandboxId"`
	Status        string    `json:"status"`
	Name          string    `json:"name"`
	EnvName       string    `json:"envName"`
	Purpose       string    `json:"purpose"`
	Routes        []route   `json:"routes"`
	Justification string    `json:"justification"`
	DecidedBy     string    `json:"decidedBy"`
	CreatedAt     time.Time `json:"createdAt"`
}

// route is a SecretRoute as pkg/secretroute owns it, and the keys of any
// narrowing secretroute does not know. A lead approves what an event says,
// so a narrowing left out would make a grant read as other than it is.
type route struct {
	secretroute.Route
	unknown []string
}

// knownRouteKeys are the SecretRoute fields secretroute.Route reads.
var knownRouteKeys = map[string]bool{"hosts": true, "methods": true, "paths": true, "blocked": true, "graphql": true, "mcp": true}

func (r *route) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &r.Route); err != nil {
		return err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return err
	}
	for key := range keys {
		if !knownRouteKeys[key] {
			r.unknown = append(r.unknown, key)
		}
	}
	slices.Sort(r.unknown)
	return nil
}

// String is the route in the form `discobox-access request --route` takes
// and every client shows (secretroute.Format), naming any narrowing it
// cannot show.
func (r route) String() string {
	text := secretroute.Format(r.Route)
	for _, key := range r.unknown {
		text += " (narrowed by " + key + ", not shown)"
	}
	return text
}

// what is the credential a request asks for, by the name it gives.
func (r secretRequest) what() string {
	switch {
	case r.Name != "" && r.EnvName != "":
		return r.Name + " (" + r.EnvName + ")"
	case r.Name != "":
		return r.Name
	case r.EnvName != "":
		return r.EnvName
	}
	return "a credential"
}

// client is the discobox API as a box reaches it: the pool's gate at
// DISCOBOX_API_URL, which the box's proxy routes and which adds the pool's
// own credential, so the box holds none (ADR 26-10-27-880 §9).
type client struct {
	base    string
	project string
	http    *http.Client
}

// apiError is a response the server refused.
type apiError struct {
	status  int
	what    string
	message string
}

func (e *apiError) Error() string {
	text := fmt.Sprintf("the discobox API refused %s (%d)", e.what, e.status)
	if e.message != "" {
		text += ": " + e.message
	}
	return text
}

// listRequests is every secret request this box may read: those of the
// boxes it created.
func (c *client) listRequests(ctx context.Context) ([]secretRequest, error) {
	var out struct {
		SecretRequests []secretRequest `json:"secretRequests"`
	}
	if err := c.get(ctx, "listing secret requests", "/projects/"+url.PathEscape(c.project)+"/secret-requests", nil, &out); err != nil {
		return nil, err
	}
	return out.SecretRequests, nil
}

// streamEvent is one event off the stream: its ID, empty for a reset; its
// type, empty for an event that only moves the cursor; and its data.
type streamEvent struct {
	id   string
	typ  string
	data boxEvent
}

// stream reads the project's sandbox events after cursor, the project's
// current state first when there is none (ADR 26-10-29-316 §4), handing each
// to onEvent until the stream ends, ctx ends, or onEvent fails. A refusal is
// an apiError; a stream that ends is not an error, since the cursor makes the
// next run lose nothing.
func (c *client) stream(ctx context.Context, cursor string, onEvent func(streamEvent) error) error {
	target := strings.TrimRight(c.base, "/") + "/api/projects/" + url.PathEscape(c.project) + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if cursor != "" {
		req.Header.Set("Last-Event-ID", cursor)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("discobox API, reading events: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return &apiError{status: resp.StatusCode, what: "reading events", message: refusal(body)}
	}
	// An error handling an event is the run's. The read ending — the run's
	// time up, the connection dropped — is not: the cursor says where to go
	// on from. Only an event too long to read is, since every run would meet
	// it again.
	var handled error
	err = sse.Read(resp.Body, nil, func(id, typ, data string) error {
		ev := streamEvent{id: id, typ: typ}
		if typ == eventCreated || typ == eventUpdated || typ == eventDeleted {
			if err := json.Unmarshal([]byte(data), &ev.data); err != nil {
				handled = fmt.Errorf("discobox API, event %s: %w", id, err)
				return handled
			}
		}
		handled = onEvent(ev)
		return handled
	})
	if handled != nil {
		return handled
	}
	if errors.Is(err, bufio.ErrTooLong) {
		return fmt.Errorf("discobox API, reading events: %w", err)
	}
	return nil
}

func (c *client) get(ctx context.Context, what, path string, query url.Values, v any) error {
	target := strings.TrimRight(c.base, "/") + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("discobox API, %s: %w", what, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("discobox API, %s: %w", what, err)
	}
	if resp.StatusCode != http.StatusOK {
		return &apiError{status: resp.StatusCode, what: what, message: refusal(body)}
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("discobox API, %s: %w", what, err)
	}
	return nil
}

// refusal is the reason a refused response gives: a problem document's
// detail or title, or the plain text the gate and the authorizer answer
// with, cut to one line.
func refusal(body []byte) string {
	var problem struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	text := string(body)
	if json.Unmarshal(body, &problem) == nil {
		text = problem.Detail
		if text == "" {
			text = problem.Title
		}
	}
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > 300 {
		text = string(r[:300])
	}
	return text
}
