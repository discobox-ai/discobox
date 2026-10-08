package sandboxruntime

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
	"github.com/discobox-ai/discobox/pool-agent/sandboxtoken"
	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/sandboxuser"
)

// A sandbox materializes its own sources (ADR 0126 §4): the pool says, in the
// runtime-config document, where each source's origin is served, what is
// pinned and where its checkout belongs, and the sandbox agent clones it,
// as the checkout's owner, in its own namespace. The pool runs no git in a
// sandbox's checkout and binds no origin into it.
//
// What the pool still owns is settling (ADR 0055): a sandbox's spec is final
// only once its primary source's project layer has been read, and a source is
// marked delivered — which is what opens the sandbox's readiness gate — only
// then. The layer is read by asking the sandbox, never by reading its tree.

const (
	// originTokenTTL is how long a source's origin token lives, and
	// originTokenRenewBefore how close to its end the pool issues the next.
	// Deciding the document renews it, and the status poll decides it every
	// fifteen seconds for a running sandbox, so a token is replaced long
	// before a fetch could meet an expired one.
	originTokenTTL         = 24 * time.Hour
	originTokenRenewBefore = 12 * time.Hour

	// sourceStatePollInterval is how often a create asks the sandbox how far
	// its sources have converged while it waits for them.
	sourceStatePollInterval = 500 * time.Millisecond
	// sourceSettleTimeout bounds that wait. It outlasts the sandbox agent's
	// own bound on one source's clone, so the agent's verdict arrives first.
	sourceSettleTimeout = 35 * time.Minute
	// sourceWaitingTimeout bounds a source the sandbox has not begun to clone
	// at all: its agent tries as soon as a document names it, so one that is
	// still waiting this long after has an origin with nothing in it.
	sourceWaitingTimeout = 2 * time.Minute
	// sourceFailureLimit is how many attempts in a row the sandbox may fail
	// to clone a source before the create gives up on it. The agent goes on
	// retrying either way; a create that waits on a source that will not come
	// fails rather than hanging.
	sourceFailureLimit = 3

	// sandboxLabelProjectLayer is the digest of the project layer a
	// container's bootstrap was built from, empty for none. Settling compares
	// it with the layer the sandbox reads from its delivered source: the
	// container says what it was built from, so a rebuild that failed part way
	// is still seen as owed.
	sandboxLabelProjectLayer = "discobox.project_layer"
)

// Source states as the sandbox agent reports them
// (sandbox-agent/sourceconverge).
const (
	sourceStateWaiting      = "waiting"
	sourceStateMaterialized = "materialized"
	sourceStateFailed       = "failed"
)

// sourceState is one source's convergence as the sandbox reports it.
type sourceState struct {
	Slug      string    `json:"slug"`
	State     string    `json:"state"`
	Commit    string    `json:"commit,omitempty"`
	Error     string    `json:"error,omitempty"`
	Revision  int64     `json:"revision"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// runtimeSources is the sandbox's sources as its document names them: where
// each checkout belongs, where its origin is served, and its pin. Whether each
// has been delivered, and its origin token, are carried over from prior, the
// document as recorded: a source is delivered once, and a token is the
// decision's to renew (refreshOriginTokens).
func (r *DockerSandboxRuntime) runtimeSources(sandboxID string, sources []sandboxSource, prior []sandboxconfig.RuntimeSource) []sandboxconfig.RuntimeSource {
	before := make(map[string]sandboxconfig.RuntimeSource, len(prior))
	for _, source := range prior {
		before[source.Slug] = source
	}
	out := make([]sandboxconfig.RuntimeSource, 0, len(sources))
	for _, source := range sources {
		origin := r.sourceOriginURL(sandboxID, source)
		next := sandboxconfig.RuntimeSource{
			Slug:      source.slug,
			Target:    source.target,
			OriginURL: origin,
			Commit:    sourceBaseCommit(source.git),
			Delivered: before[source.slug].Delivered,
		}
		if previous := before[source.slug]; previous.OriginURL == origin {
			next.OriginToken = previous.OriginToken
		}
		out = append(out, next)
	}
	return out
}

// sourceOriginURL is where the sandbox fetches a source from: this pool's
// git-origins route, at the origins host its proxy answers, for a source the
// client delivers from its own machine — live from the developer's Git
// directory, or the bare origin it pushes into — and the remote itself for a
// remote-URL source.
func (r *DockerSandboxRuntime) sourceOriginURL(sandboxID string, source sandboxSource) string {
	if poolServesOrigin(source.git) {
		return proxyagent.OriginURL(r.projectID, r.poolID, sandboxID, source.slug)
	}
	if remote, ok := source.git.URL.Get(); ok {
		return remote.String()
	}
	return ""
}

// poolServesOrigin reports whether a source's origin is this pool's to serve:
// one pushed into the pool, or one in a directory on the client's machine.
func poolServesOrigin(source workerapimodel.GitSource) bool {
	return gitSourceAwaitsPush(source) || strings.TrimSpace(optString(source.LocalDirectory)) != ""
}

// refreshOriginTokens gives every source whose origin this pool serves a token
// that fetches it, keeping the one the document holds until it is near its
// end. The token is this pool's sandbox token, scope origin:fetch, which reads
// this sandbox's origins and nothing else (sandboxtoken).
func (r *DockerSandboxRuntime) refreshOriginTokens(sandboxID string, doc *sandboxconfig.RuntimeConfig) error {
	if len(r.identityKey) == 0 {
		return nil
	}
	var verifier *sandboxtoken.Verifier
	for i := range doc.Sources {
		source := &doc.Sources[i]
		if !strings.HasPrefix(source.OriginURL, "https://"+proxyagent.OriginsHost+"/") {
			source.OriginToken = ""
			continue
		}
		if source.OriginToken != "" {
			if verifier == nil {
				public, _ := r.identityKey.Public().(ed25519.PublicKey)
				v, err := sandboxtoken.NewVerifier(public)
				if err != nil {
					return err
				}
				verifier = v
			}
			if claims, err := verifier.Verify(source.OriginToken); err == nil &&
				claims.SandboxID == sandboxID && time.Until(claims.Expires) > originTokenRenewBefore {
				continue
			}
		}
		token, err := sandboxtoken.Issue(r.identityKey, sandboxtoken.Claims{
			ProjectID: r.projectID,
			PoolID:    r.poolID,
			SandboxID: sandboxID,
			Scopes:    []string{sandboxtoken.ScopeOriginFetch},
		}, originTokenTTL)
		if err != nil {
			return fmt.Errorf("issue origin token for source %q: %w", source.Slug, err)
		}
		source.OriginToken = token
	}
	return nil
}

// settleSources finishes a create whose container is up: it waits for the
// sandbox to materialize every source its document names, reads the primary
// source's project layer from it, and either marks the sources delivered —
// which opens the sandbox's readiness gate — or reports that the container
// has to be rebuilt against a bootstrap that carries that layer (ADR 0055).
// The layer it read is recorded first, so the rebuild uses it.
//
// Nothing is settled while a push-delivered source's origin is still empty:
// the sandbox parks, and the resume create after the client's push settles it.
// A sandbox that is not running is left to the convergence after its next
// start (settleConverged), since only a running sandbox can answer.
func (r *DockerSandboxRuntime) settleSources(ctx context.Context, sb *Sandbox, req *workerapimodel.PoolSandboxCreateRequest) (bool, error) {
	sandboxID := sb.SandboxID
	doc, ok, err := r.readRuntimeConfig(sandboxID)
	if err != nil || !ok || doc.SourcesDelivered() || sb.Status != StatusRunning {
		return false, err
	}
	sources := sandboxSources(r.paths, req)
	landed, err := r.pushedSourcesLanded(ctx, sandboxID, sources, resolveSandboxUser(r.paths, req))
	if err != nil || !landed {
		return false, err
	}
	// Delivered again, so a sandbox whose agent is backing off from an origin
	// that was empty until now goes again at once: a delivery wakes it, the
	// same document or not.
	if err := r.deliverRuntimeConfig(ctx, sandboxID, nil); err != nil {
		if errors.Is(err, ErrRuntimeConfigUnsupported) || errors.Is(err, ErrRuntimeConfigRefused) {
			return false, err
		}
		slog.WarnContext(ctx, "deliver sandbox runtime config before settling its sources", "sandboxId", sandboxID, "error", err)
	}
	dial, err := r.SandboxDialer(ctx, sandboxID, SandboxAgentPort)
	if err != nil {
		return false, err
	}
	if err := r.awaitSourcesMaterialized(ctx, dial, sandboxID, doc.Sources); err != nil {
		return false, err
	}
	built, err := r.containerProjectLayerDigest(ctx, sb.ID)
	if err != nil {
		return false, err
	}
	// The primary's slug is the request's, not the record's: a record from
	// before it named the slug names none.
	primary := ""
	if _, hasPrimary := req.Config.Source.Get(); hasPrimary {
		primary = sources[0].slug
	}
	rebuild, err := r.settleProjectLayer(ctx, dial, sandboxID, primary, built)
	if err != nil || rebuild {
		return rebuild, err
	}
	return false, r.markSourcesDelivered(ctx, sandboxID)
}

// settleConverged is settleSources for a running sandbox no create is waiting
// on — one started rather than created, such as an unarchived or imported
// sandbox, or a sandbox whose create stopped short of settling. The status
// poll calls it through ConvergeRuntimeConfig while the record names a source
// not yet delivered. It does not wait: a source the sandbox has not
// materialized yet is looked at again on the next poll. With no create
// request in hand it cannot rebuild the container itself, so a project layer
// that needs one removes the container for the control plane to recreate,
// on the bootstrap the recorded layer now gives it.
func (r *DockerSandboxRuntime) settleConverged(ctx context.Context, sb *Sandbox) error {
	doc, ok, err := r.readRuntimeConfig(sb.SandboxID)
	if err != nil || !ok || doc.SourcesDelivered() {
		return err
	}
	dial, err := r.SandboxDialer(ctx, sb.SandboxID, SandboxAgentPort)
	if err != nil {
		return err
	}
	states, err := r.readSourceStates(ctx, dial, sb.SandboxID)
	if errors.Is(err, ErrSourceStatesUnsupported) {
		// Nothing this poll can do; the create that rebuilds the sandbox on a
		// current image says so.
		return nil
	}
	if err != nil {
		return err
	}
	for _, source := range doc.Sources {
		if states[source.Slug].State != sourceStateMaterialized {
			return nil
		}
	}
	// The sandbox may have cloned a pushed source the moment the push landed,
	// before the create that resumes it: its origin's HEAD has to name what
	// arrived before the source is settled, as the resume would have made it.
	if err := r.headPushedOrigins(ctx, sb.SandboxID, doc.Sources); err != nil {
		return err
	}
	built, err := r.containerProjectLayerDigest(ctx, sb.ID)
	if err != nil {
		return err
	}
	record, err := r.readProjectLayerRecord(sb.SandboxID)
	if err != nil || record.Source == nil {
		return err
	}
	rebuild, err := r.settleProjectLayer(ctx, dial, sb.SandboxID, *record.Source, built)
	if err != nil {
		return err
	}
	if !rebuild {
		return r.markSourcesDelivered(ctx, sb.SandboxID)
	}
	slog.InfoContext(ctx, "removing a sandbox container whose delivered source declares project configuration it was not built with; the control plane rebuilds it", "sandboxId", sb.SandboxID)
	return r.removeForRebuild(ctx, sb)
}

// settleProjectLayer reads the primary source's project layer from a sandbox
// whose sources are all materialized — none when primary, its slug, is empty
// — and reports whether the container, built with the layer whose digest is
// built, has to be rebuilt for it. A layer that calls for a rebuild is
// recorded, so the rebuild's bootstrap carries it.
func (r *DockerSandboxRuntime) settleProjectLayer(ctx context.Context, dial Dialer, sandboxID, primary, built string) (bool, error) {
	var layer *sandboxconfig.ProjectLayer
	if primary != "" {
		var err error
		if layer, err = r.readSourceProjectLayer(ctx, dial, sandboxID, primary); err != nil {
			return false, err
		}
	}
	if projectLayerDigest(layer) == built {
		return false, nil
	}
	return true, r.writeProjectLayerRecord(sandboxID, projectLayerRecord{Project: layer, Source: &primary})
}

// markSourcesDelivered tells the sandbox its spec is final and its sources
// delivered, which is what clears its readiness gate (ADR 0055).
func (r *DockerSandboxRuntime) markSourcesDelivered(ctx context.Context, sandboxID string) error {
	return logRuntimeConfigFailure(ctx, sandboxID, r.deliverRuntimeConfig(ctx, sandboxID, func(doc *sandboxconfig.RuntimeConfig) {
		for i := range doc.Sources {
			doc.Sources[i].Delivered = true
		}
	}))
}

// pushedSourcesLanded reports whether every push-delivered source's origin
// holds the client's push, and gives each one that does the HEAD its clone
// reads. It runs git in the pool's own bare origin, never in the sandbox's
// checkout.
func (r *DockerSandboxRuntime) pushedSourcesLanded(ctx context.Context, sandboxID string, sources []sandboxSource, user sandboxuser.User) (bool, error) {
	for _, source := range sources {
		if !gitSourceAwaitsPush(source.git) {
			continue
		}
		origin := r.sandboxOriginPath(sandboxID, source.slug)
		uid, gid := chownID(user.UID), chownID(user.GID)
		if !gitOriginHasRefs(ctx, origin, uid, gid) {
			// The client has not pushed yet: the sandbox parks until the push
			// is reported complete and create runs again (ADR 0001 §4).
			return false, nil
		}
		if err := ensureOriginHead(ctx, origin, source.git, uid, gid); err != nil {
			return false, fmt.Errorf("source %q origin: %w", source.slug, err)
		}
	}
	return true, nil
}

// headPushedOrigins points the HEAD of each of the sandbox's bare origins that
// has received a push at what arrived, as the origin's owner
// (headOriginAtWhatArrived). A source with no bare origin is not pushed.
func (r *DockerSandboxRuntime) headPushedOrigins(ctx context.Context, sandboxID string, sources []sandboxconfig.RuntimeSource) error {
	for _, source := range sources {
		origin := r.sandboxOriginPath(sandboxID, source.Slug)
		info, err := os.Stat(origin)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		uid, gid := fileOwner(info)
		if err := headOriginAtWhatArrived(ctx, origin, uid, gid); err != nil {
			return fmt.Errorf("source %q origin: %w", source.Slug, err)
		}
	}
	return nil
}

// ErrSourceStatesUnsupported is a sandbox agent that cannot say how far its
// sources have converged: an image whose agent predates the route, or one with
// no source converger. Waiting on it would wait forever.
var ErrSourceStatesUnsupported = errors.New("the sandbox's image predates cloning its own sources; rebuild it on a current discobox base")

// awaitSourcesMaterialized waits until the sandbox reports every source named
// materialized. A source whose clone keeps failing, or that the sandbox never
// starts on, fails the wait with what the sandbox said.
func (r *DockerSandboxRuntime) awaitSourcesMaterialized(ctx context.Context, dial Dialer, sandboxID string, sources []sandboxconfig.RuntimeSource) error {
	ctx, cancel := context.WithTimeout(ctx, sourceSettleTimeout)
	defer cancel()
	start := time.Now()
	// The first failure seen may be from before this wait woke the agent, so
	// only a failure reported later counts, and only a run of them.
	firstSeen := map[string]sourceState{}
	failures := map[string]int{}
	for {
		states, err := r.readSourceStates(ctx, dial, sandboxID)
		if errors.Is(err, ErrSourceStatesUnsupported) {
			return fmt.Errorf("sandbox %s: %w", sandboxID, err)
		}
		if err != nil {
			slog.WarnContext(ctx, "read sandbox source states", "sandboxId", sandboxID, "error", err)
		}
		pending := 0
		for _, source := range sources {
			state, ok := states[source.Slug]
			if ok && state.State == sourceStateMaterialized {
				continue
			}
			pending++
			if !ok {
				continue
			}
			first, seen := firstSeen[source.Slug]
			if !seen {
				firstSeen[source.Slug] = state
				first = state
			}
			switch {
			case state.State == sourceStateFailed && !state.UpdatedAt.Equal(first.UpdatedAt):
				failures[source.Slug]++
				firstSeen[source.Slug] = state
				if failures[source.Slug] >= sourceFailureLimit {
					return fmt.Errorf("materialize source %q: %s", source.Slug, state.Error)
				}
			case state.State == sourceStateWaiting && time.Since(start) > sourceWaitingTimeout:
				return fmt.Errorf("materialize source %q: its origin %s has nothing to clone", source.Slug, source.OriginURL)
			}
		}
		if pending == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for the sandbox to materialize its sources: %w", ctx.Err())
		case <-time.After(sourceStatePollInterval):
		}
	}
}

// readSourceStates asks the sandbox how far each source has converged.
func (r *DockerSandboxRuntime) readSourceStates(ctx context.Context, dial Dialer, sandboxID string) (map[string]sourceState, error) {
	var body struct {
		Sources []sourceState `json:"sources"`
	}
	if err := r.getFromSandboxAgent(ctx, dial, sandboxID, "/sources", &body); err != nil {
		return nil, err
	}
	out := make(map[string]sourceState, len(body.Sources))
	for _, state := range body.Sources {
		out[state.Slug] = state
	}
	return out, nil
}

// readSourceProjectLayer asks the sandbox for a materialized source's project
// layer, nil when the source has none (ADR 0012 §7).
func (r *DockerSandboxRuntime) readSourceProjectLayer(ctx context.Context, dial Dialer, sandboxID, slug string) (*sandboxconfig.ProjectLayer, error) {
	var body struct {
		ProjectLayer json.RawMessage `json:"projectLayer"`
	}
	if err := r.getFromSandboxAgent(ctx, dial, sandboxID, "/sources/"+slug+"/project-layer", &body); err != nil {
		return nil, fmt.Errorf("read source %q project layer: %w", slug, err)
	}
	if len(body.ProjectLayer) == 0 || string(body.ProjectLayer) == "null" {
		return nil, nil
	}
	var layer sandboxconfig.ProjectLayer
	if err := json.Unmarshal(body.ProjectLayer, &layer); err != nil {
		return nil, fmt.Errorf("parse source %q .discobox/project.json: %w", slug, err)
	}
	return &layer, nil
}

// getFromSandboxAgent reads one of the sandbox agent's runtime-config routes,
// with the token the pool signs for them (runtimeConfigToken).
func (r *DockerSandboxRuntime) getFromSandboxAgent(ctx context.Context, dial Dialer, sandboxID, route string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, runtimeConfigCallTimeout)
	defer cancel()
	token, err := r.runtimeConfigToken(sandboxID)
	if err != nil {
		return err
	}
	target := HTTPURL(SandboxAgentPort, fmt.Sprintf("/api/projects/%s/sandboxes/%s%s", r.projectID, sandboxID, route))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Transport: dial.Transport()}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("sandbox %s answered %s with status %d: %s", sandboxID, route, resp.StatusCode, strings.TrimSpace(string(data)))
		// The router's bare 404 for a route it does not have, and the 503 of
		// an agent with no converger, are answers no retry changes.
		if route == "/sources" && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusServiceUnavailable) {
			return fmt.Errorf("%w: %w", ErrSourceStatesUnsupported, err)
		}
		return err
	}
	return json.Unmarshal(data, out)
}

// projectLayerDigest names a project layer for a container label: the SHA-256
// of its JSON, and empty for none, which is what a container built before the
// label existed carries too.
func projectLayerDigest(layer *sandboxconfig.ProjectLayer) string {
	if layer == nil {
		return ""
	}
	data, err := json.Marshal(layer)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// containerProjectLayerDigest is the project layer digest a container was
// built with.
func (r *DockerSandboxRuntime) containerProjectLayerDigest(ctx context.Context, containerID string) (string, error) {
	inspect, err := r.client.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return "", ErrNotFound
		}
		return "", err
	}
	if inspect.Container.Config == nil {
		return "", nil
	}
	return inspect.Container.Config.Labels[sandboxLabelProjectLayer], nil
}

// projectLayerRecordName is the pool's record of a sandbox's project layer, in
// the sandbox's tree root beside — not inside — the volumes it mounts: the
// layer the pool has decided the sandbox's bootstrap is built from, and which
// source it is read from.
const projectLayerRecordName = "project-layer.json"

type projectLayerRecord struct {
	Project *sandboxconfig.ProjectLayer `json:"project,omitempty"`
	// Source is the slug the layer is read from: the primary source's, and
	// empty when the sandbox has no primary source and so no layer. Nil is a
	// record written before it named one, by a pool that read the layer from
	// the checkout itself: only a create, which has the request, can say.
	Source *string `json:"source"`
}

func (r *DockerSandboxRuntime) projectLayerRecordPath(sandboxID string) string {
	return filepath.Join(r.sandboxRoot(sandboxID), projectLayerRecordName)
}

// readProjectLayerRecord is the sandbox's project layer record; none is the
// zero record. It is the pool's own, never sandbox.json's _provenance read
// back: that file is the sandbox's once it boots, and nothing the pool
// decides is read from what a sandbox can write (ADR 26-10-08-127).
func (r *DockerSandboxRuntime) readProjectLayerRecord(sandboxID string) (projectLayerRecord, error) {
	data, err := os.ReadFile(r.projectLayerRecordPath(sandboxID))
	if err != nil {
		if os.IsNotExist(err) {
			return projectLayerRecord{}, nil
		}
		return projectLayerRecord{}, err
	}
	var record projectLayerRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return projectLayerRecord{}, fmt.Errorf("parse %s: %w", projectLayerRecordName, err)
	}
	return record, nil
}

func (r *DockerSandboxRuntime) writeProjectLayerRecord(sandboxID string, record projectLayerRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	path := r.projectLayerRecordPath(sandboxID)
	if err := os.MkdirAll(r.sandboxRoot(sandboxID), 0o755); err != nil {
		return fmt.Errorf("record project layer: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("record project layer: %w", err)
	}
	return os.Rename(tmp, path)
}
