package discoboxbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/programstatus"
	"github.com/discobox-ai/discobox/pkg/sandboxmeta"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// box is what this monitor reads of a Sandbox in a listing. A local struct,
// not `api/gen`'s type: runtime code does not use the public API's DTOs
// (the root REVIEW.md), and a field a newer server adds breaks nothing.
type box struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	Config    struct {
		Name string `json:"name"`
	} `json:"config"`
	Meta struct {
		Name string            `json:"name"`
		Tags map[string]string `json:"tags"`
	} `json:"meta"`
	Runtime *struct {
		DisplayState string `json:"displayState"`
		AgentStatus  *struct {
			Sessions json.RawMessage `json:"sessions"`
		} `json:"agentStatus"`
	} `json:"runtime"`
}

// name is what an event calls the box: its title as every client resolves
// it (sandboxmeta.Title) — the name in its meta, else its primary terminal's
// window title, else its configured name or ID — without the msg the server's
// displayName carries, which changes with every turn.
func (b box) name() string {
	var title string
	if b.Runtime != nil && b.Runtime.AgentStatus != nil {
		title, _ = programstatus.PrimaryTitleAndMsg(b.Runtime.AgentStatus.Sessions)
	}
	fallback := strings.TrimSpace(b.Config.Name)
	if fallback == "" {
		fallback = b.ID
	}
	return sandboxmeta.Title(b.Meta.Name, title, fallback, "")
}

func (b box) displayState() string {
	if b.Runtime == nil || b.Runtime.DisplayState == "" {
		return "pending"
	}
	return b.Runtime.DisplayState
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

func (c *client) listBoxes(ctx context.Context, tags []string) ([]box, error) {
	query := url.Values{}
	for _, tag := range tags {
		query.Add("tag", tag)
	}
	var out struct {
		Sandboxes []box `json:"sandboxes"`
	}
	if err := c.get(ctx, "listing boxes", "/projects/"+url.PathEscape(c.project)+"/sandboxes", query, &out); err != nil {
		return nil, err
	}
	return out.Sandboxes, nil
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
