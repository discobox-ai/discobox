// Package sourceconverge materializes the sandbox's own sources (ADR 0126 §4):
// the runtime-config document says, per source, where its origin is served,
// what is pinned and where the checkout belongs, and the agent converges each
// one onto its target and reports how far it got.
//
// Materializing is convergence, not a verb. A source is cloned, checked out
// and its workspace snapshot restored exactly once, then marked inside its own
// .git (sandboxconfig.SourceMaterializedMarker); every later pass only asserts
// the origin remote, because re-materializing a workspace the sandbox has been
// using would discard its work. git runs as whoever owns the checkout, in the
// sandbox's own namespace, so nothing is chowned after it and no
// safe.directory is needed.
//
// A materialized source is not the ready signal. The pool still reads the
// delivered project layer (ProjectLayer), settles the sandbox's spec, and only
// then says so with the document's Delivered (ADR 0055).
package sourceconverge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

// State is how far a source has converged.
type State string

const (
	// StateWaiting is a source with nothing to clone yet: no origin, or an
	// origin the client has not pushed anything into.
	StateWaiting State = "waiting"
	// StateCloning is a source being materialized now.
	StateCloning State = "cloning"
	// StateMaterialized is a source cloned, checked out and restored, once.
	StateMaterialized State = "materialized"
	// StateFailed is a source whose last attempt failed; it is retried.
	StateFailed State = "failed"
)

// SourceState is one source's convergence, as the status poll reports it.
type SourceState struct {
	Slug  string
	State State
	// Commit is the commit checked out, once materialized.
	Commit string
	// Error is why the last attempt failed.
	Error string
	// Progress is how far the clone has got while State is cloning, nil
	// until git has said.
	Progress *CloneProgress
	// Revision is the runtime-config revision the state was reached under.
	Revision  int64
	UpdatedAt time.Time
}

// Config is what a Converger needs from the sandbox around it.
type Config struct {
	// Manifest is sandbox.json's sources: how each slug is checked out — its
	// branch or tag, its upstream remote, its workspace snapshot.
	Manifest []sandboxconfig.Source
	// Env is the sandbox's environment, which git runs with so that a clone
	// takes the same route off the box — the pool proxy — a user's git does.
	Env map[string]string
	// User is the sandbox's resolved default user. git runs as the owner of
	// each checkout, and as this user, with its home and groups, when that is
	// who the owner is.
	User *execs.User
	// CredentialHelper is the credential.helper value each origin is
	// configured with, so a fetch — the agent's own and a user's later
	// `git fetch origin` alike — reads the origin's token from this agent.
	// Empty configures none.
	CredentialHelper string
	// RetryInterval is the first wait before a failed or waiting source is
	// tried again; it doubles to MaxRetryInterval. Zero uses the defaults.
	RetryInterval    time.Duration
	MaxRetryInterval time.Duration
	Logger           *slog.Logger
}

const (
	defaultRetryInterval    = 5 * time.Second
	defaultMaxRetryInterval = 2 * time.Minute
	// sourceTimeout bounds one source's materialization, a clone of a large
	// repository through the proxy included.
	sourceTimeout = 30 * time.Minute
)

// Converger converges the sandbox's sources on the last document it was given.
type Converger struct {
	cfg      Config
	manifest map[string]sandboxconfig.Source
	wake     chan struct{}

	mu      sync.Mutex
	desired *sandboxconfig.RuntimeConfig
	// received is when desired arrived, which is when a source not tried
	// yet started waiting.
	received time.Time
	states   map[string]SourceState
}

// New returns a Converger. Nothing happens until Run is running and Converge
// has been given a document.
func New(cfg Config) *Converger {
	if cfg.RetryInterval <= 0 {
		cfg.RetryInterval = defaultRetryInterval
	}
	if cfg.MaxRetryInterval < cfg.RetryInterval {
		cfg.MaxRetryInterval = max(defaultMaxRetryInterval, cfg.RetryInterval)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	manifest := make(map[string]sandboxconfig.Source, len(cfg.Manifest))
	for _, source := range cfg.Manifest {
		manifest[source.Slug] = source
	}
	return &Converger{
		cfg:      cfg,
		manifest: manifest,
		wake:     make(chan struct{}, 1),
		states:   map[string]SourceState{},
	}
}

// Converge makes doc the document to converge on and wakes the loop. It never
// blocks: a document delivered while a pass runs is picked up by the next.
func (c *Converger) Converge(doc sandboxconfig.RuntimeConfig) {
	c.mu.Lock()
	c.desired, c.received = &doc, time.Now().UTC()
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Run converges until ctx ends: on every new document, and again after a
// growing interval while any source is failed or waiting on its origin.
func (c *Converger) Run(ctx context.Context) {
	var retry <-chan time.Time
	backoff := c.cfg.RetryInterval
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
			backoff = c.cfg.RetryInterval
		case <-retry:
		}
		if c.pass(ctx) {
			retry = time.After(backoff)
			backoff = min(backoff*2, c.cfg.MaxRetryInterval)
		} else {
			retry = nil
			backoff = c.cfg.RetryInterval
		}
	}
}

// States reports each source the current document names, in its order.
func (c *Converger) States() []SourceState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.desired == nil {
		return nil
	}
	out := make([]SourceState, 0, len(c.desired.Sources))
	for _, source := range c.desired.Sources {
		if state, ok := c.states[source.Slug]; ok {
			out = append(out, state)
			continue
		}
		out = append(out, SourceState{Slug: source.Slug, State: StateWaiting, Revision: c.desired.Revision, UpdatedAt: c.received})
	}
	return out
}

// pass converges every source once and reports whether one is worth trying
// again without a new document.
func (c *Converger) pass(ctx context.Context) bool {
	c.mu.Lock()
	doc := c.desired
	c.mu.Unlock()
	if doc == nil {
		return false
	}
	again := false
	for _, source := range doc.Sources {
		if ctx.Err() != nil {
			return false
		}
		state := c.converge(ctx, doc.Revision, source)
		// A source still cloning after its pass was superseded mid-attempt;
		// failed and waiting ones are retried until they converge.
		if state.State == StateFailed || state.State == StateCloning || (state.State == StateWaiting && source.OriginURL != "") {
			again = true
		}
	}
	return again
}

func (c *Converger) setState(state SourceState) SourceState {
	state.UpdatedAt = time.Now().UTC()
	c.mu.Lock()
	c.states[state.Slug] = state
	c.mu.Unlock()
	return state
}

// converge brings one source to its target and records where it got.
func (c *Converger) converge(ctx context.Context, revision int64, source sandboxconfig.RuntimeSource) SourceState {
	state := SourceState{Slug: source.Slug, Revision: revision}
	if source.OriginURL == "" {
		state.State = StateWaiting
		return c.setState(state)
	}
	ctx, cancel := context.WithTimeout(ctx, sourceTimeout)
	defer cancel()
	repo, err := c.open(source.Target)
	if err != nil {
		return c.failed(state, err)
	}
	// Only an origin that takes a token is handed to the agent's helper: the
	// helper configuration clears every other helper for its URL, and a
	// remote-URL source's user may well have their own.
	helper := ""
	if source.OriginToken != "" {
		helper = c.cfg.CredentialHelper
	}
	if commit, ok := materializedAt(source.Target); ok {
		// Once materialized the workspace is the sandbox's: only the origin
		// remote is asserted, since the pool may move where it serves it.
		if err := repo.ensureOrigin(ctx, source.OriginURL, helper); err != nil {
			return c.failed(state, err)
		}
		repo.ensureOriginHeadRef(ctx)
		state.State, state.Commit = StateMaterialized, commit
		return c.setState(state)
	}
	if commit, ok := repo.unmarkedCheckout(ctx); ok {
		// A checkout made before materializing was marked at all — a pool
		// cloned it, from before the marker existed, into a sandbox that has
		// not started since (unmarkedCheckout says how it is told). It is the
		// sandbox's workspace, so it is adopted as it stands rather than
		// refused or cloned over: marked once, at what it has checked out,
		// and from then on like any other.
		if err := repo.ensureOrigin(ctx, source.OriginURL, helper); err != nil {
			return c.failed(state, err)
		}
		if err := repo.writeMarker(filepath.Join(source.Target, ".git", sandboxconfig.SourceMaterializedMarker), commit); err != nil {
			return c.failed(state, err)
		}
		c.cfg.Logger.Info("adopted a source checkout from before the materialized marker", "slug", source.Slug, "target", source.Target, "commit", commit)
		state.State, state.Commit = StateMaterialized, commit
		return c.setState(state)
	}
	state.State = StateCloning
	cloning := c.setState(state)
	cloned, err := repo.prepare(ctx, source, c.manifest[source.Slug], helper, func(progress CloneProgress) {
		c.cloneProgress(cloning, progress)
	})
	if err != nil {
		return c.failed(state, err)
	}
	if !cloned {
		state.State = StateWaiting
		return c.setState(state)
	}
	// Marking it materialized is once-only, so it is done only for the source
	// the current document still describes. A document that moved its pin,
	// origin or target while this attempt ran has woken the loop already; the
	// next pass goes on from this checkout to the new pin (the resume path
	// fetches and checks out again), and this one finishes nothing. The check
	// and the marker are one step under the lock that Converge takes.
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stillDesired(source) {
		return state
	}
	commit, err := repo.finish(ctx)
	state.UpdatedAt = time.Now().UTC()
	if err != nil {
		state.State, state.Error = StateFailed, err.Error()
		c.states[state.Slug] = state
		return state
	}
	state.State, state.Commit = StateMaterialized, commit
	c.states[state.Slug] = state
	c.cfg.Logger.Info("materialized source", "slug", source.Slug, "target", source.Target, "commit", commit)
	return state
}

// stillDesired reports whether the current document describes source as it
// was when an attempt began: the same target, origin and pin. A new token
// alone does not matter to what was cloned. The caller holds c.mu.
func (c *Converger) stillDesired(source sandboxconfig.RuntimeSource) bool {
	if c.desired == nil {
		return false
	}
	for _, current := range c.desired.Sources {
		if current.Slug == source.Slug {
			return current.Target == source.Target && current.OriginURL == source.OriginURL && current.Commit == source.Commit
		}
	}
	return false
}

// cloneProgress records how far the clone that set state has got. The state
// it set is still the one recorded unless the attempt has ended, and an ended
// attempt's last meter is not news.
func (c *Converger) cloneProgress(state SourceState, progress CloneProgress) {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.states[state.Slug]
	if !ok || current.State != StateCloning || !current.UpdatedAt.Equal(state.UpdatedAt) {
		return
	}
	current.Progress = &progress
	c.states[state.Slug] = current
}

func (c *Converger) failed(state SourceState, err error) SourceState {
	state.State, state.Error = StateFailed, err.Error()
	c.cfg.Logger.Warn("materialize source", "slug", state.Slug, "error", err)
	return c.setState(state)
}

// open is the checkout at target, worked on as whoever owns it.
func (c *Converger) open(target string) (*repository, error) {
	info, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("source target: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("source target %s is not a directory", target)
	}
	return &repository{dir: target, env: c.cfg.Env, owner: c.owner(info)}, nil
}

// owner is the identity to run git as in a directory described by info: this
// process's own when it owns it, the sandbox's resolved user — with its home
// and groups — when that is who does, and otherwise the bare ids.
func (c *Converger) owner(info fs.FileInfo) *execs.User {
	uid, gid, ok := fileOwner(info)
	if !ok || uid == int64(os.Getuid()) {
		return nil
	}
	if user := c.cfg.User; user != nil && user.UID != nil && *user.UID == uid {
		return user.Clone()
	}
	return &execs.User{UID: &uid, GID: &gid}
}

// Token is the bearer token for an origin, matched the way git describes the
// request to a credential helper: scheme, host (with any port) and path. It is
// empty when no source in the current document is served there with one.
func (c *Converger) Token(protocol, host, requestPath string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.desired == nil {
		return ""
	}
	for _, source := range c.desired.Sources {
		if source.OriginToken == "" {
			continue
		}
		origin, err := url.Parse(source.OriginURL)
		if err != nil {
			continue
		}
		if strings.EqualFold(origin.Scheme, protocol) && strings.EqualFold(origin.Host, host) &&
			strings.Trim(origin.Path, "/") == strings.Trim(requestPath, "/") {
			return source.OriginToken
		}
	}
	return ""
}

// ErrUnknownSource is a slug the current document does not name.
var ErrUnknownSource = errors.New("no such source")

// ErrNotMaterialized is a source the sandbox has not materialized yet.
var ErrNotMaterialized = errors.New("source is not materialized")

// ErrInvalidProjectLayer is a project layer file that is not a JSON object of
// a sane size.
var ErrInvalidProjectLayer = errors.New("invalid project layer")

// projectLayerPath is the project layer inside a source's working tree, and
// maxProjectLayer the most of it that is read.
const (
	projectLayerPath = ".discobox/project.json"
	maxProjectLayer  = 1 << 20
)

// ProjectLayer is a materialized source's project layer as the sandbox read it.
type ProjectLayer struct {
	Slug   string
	Commit string
	// Layer is the file's JSON object, nil when the source has none.
	Layer json.RawMessage
}

// ProjectLayer reads slug's .discobox/project.json from its working tree, for
// the pool to settle the sandbox's spec on (ADR 0055). It is read through an
// os.Root at the checkout, so a symlink in the repository cannot point this
// root-run read anywhere outside it.
func (c *Converger) ProjectLayer(ctx context.Context, slug string) (ProjectLayer, error) {
	c.mu.Lock()
	var source *sandboxconfig.RuntimeSource
	if c.desired != nil {
		for i := range c.desired.Sources {
			if c.desired.Sources[i].Slug == slug {
				found := c.desired.Sources[i]
				source = &found
				break
			}
		}
	}
	c.mu.Unlock()
	if source == nil || source.Target == "" {
		return ProjectLayer{}, fmt.Errorf("%w: %q", ErrUnknownSource, slug)
	}
	if _, ok := materializedAt(source.Target); !ok {
		return ProjectLayer{}, fmt.Errorf("%w: %q", ErrNotMaterialized, slug)
	}
	repo, err := c.open(source.Target)
	if err != nil {
		return ProjectLayer{}, err
	}
	out := ProjectLayer{Slug: slug, Commit: repo.head(ctx)}
	root, err := os.OpenRoot(source.Target)
	if err != nil {
		return ProjectLayer{}, err
	}
	defer root.Close()
	file, err := root.Open(projectLayerPath)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return ProjectLayer{}, fmt.Errorf("%w: %w", ErrInvalidProjectLayer, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxProjectLayer+1))
	if err != nil {
		return ProjectLayer{}, fmt.Errorf("%w: %w", ErrInvalidProjectLayer, err)
	}
	if len(data) > maxProjectLayer {
		return ProjectLayer{}, fmt.Errorf("%w: %s is over %d bytes", ErrInvalidProjectLayer, projectLayerPath, maxProjectLayer)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return ProjectLayer{}, fmt.Errorf("%w: %s is not a JSON object", ErrInvalidProjectLayer, projectLayerPath)
	}
	out.Layer = data
	return out, nil
}

// materializedAt reports whether the checkout at target carries the marker
// that it has been materialized once, and the commit the marker records — empty
// for a marker a pool wrote, which records none.
func materializedAt(target string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(target, ".git", sandboxconfig.SourceMaterializedMarker))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}
