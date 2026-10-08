package sandboxruntime

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"aidanwoods.dev/go-paseto"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

// The runtime-config document is the dynamic half of what a sandbox is: what
// can change while it exists — its secrets, its proxy credential, the pool's
// idle timeout, and whether its sources are delivered. The pool decides it,
// keeps its own record of the decision, and delivers it to the sandbox agent's
// intake, which converges on it (ADR 0126 §3, ADR 26-10-08-127). The static
// half is the bootstrap, sandbox.json, placed once before the container exists
// and never rewritten.
//
// Nothing here writes into a sandbox's volumes. The record lives in the
// sandbox's tree beside — never inside — what the sandbox mounts, so it
// survives archiving and leaves with a delete, and it is the pool's own: what
// the sandbox has applied is learned by asking it.

const (
	// runtimeConfigRecordName is the pool's record of the document it has
	// decided for a sandbox, in the sandbox's tree root.
	runtimeConfigRecordName = "runtime-config.json"

	// runtimeConfigTokenTTL bounds a delivery token. Each delivery signs its
	// own, so it only has to outlive one request.
	runtimeConfigTokenTTL = 5 * time.Minute
	// runtimeConfigCallTimeout bounds one delivery, which includes the sandbox
	// starting the units the material it carries feeds.
	runtimeConfigCallTimeout = 2 * time.Minute

	// sandboxAgentAudience is the audience every sandbox-agent token carries,
	// as the sandbox agent requires it (sandbox-agent/server).
	sandboxAgentAudience = "sandbox-agent"

	// sandboxLabelRuntimeConfig marks a container whose bootstrap names this
	// pool's key, so its sandbox agent takes the pool's runtime-config
	// documents. A container without it was built from a bootstrap that did
	// not, and runs on the files staged for it before then until a create
	// replaces it (containerSpecDrifted).
	sandboxLabelRuntimeConfig = "discobox.runtime_config"
)

// ErrRuntimeConfigUnsupported is a sandbox agent with no runtime-config intake:
// an image built on a base from before it existed. The pool does not stage
// files around it (ADR 26-10-08-127 §7).
var ErrRuntimeConfigUnsupported = errors.New("the sandbox's image predates the runtime-config intake; rebuild it on a current discobox base")

// ErrRuntimeConfigRefused is a sandbox agent that answered a delivery with a
// refusal no retry can change: the pool's token not accepted (an image whose
// agent predates pool-signed delivery, ADR 26-10-08-127 §3), or the document
// itself refused.
var ErrRuntimeConfigRefused = errors.New("the sandbox refused the pool's runtime config")

// runtimeConfigRecord is the record file: the document as decided, without the
// client key, which the pool keeps with the rest of the sandbox's proxy
// material and adds when it delivers.
type runtimeConfigRecord struct {
	Document sandboxconfig.RuntimeConfig `json:"document"`
}

func (r *DockerSandboxRuntime) runtimeConfigRecordPath(sandboxID string) string {
	return filepath.Join(r.sandboxRoot(sandboxID), runtimeConfigRecordName)
}

// runtimeConfigLock serializes deciding and delivering one sandbox's document,
// so two deliveries cannot each take the other's revision.
func (r *DockerSandboxRuntime) runtimeConfigLock(sandboxID string) *sync.Mutex {
	actual, _ := r.runtimeConfigLocks.LoadOrStore(sandboxID, &sync.Mutex{})
	lock, _ := actual.(*sync.Mutex)
	return lock
}

// readRuntimeConfig is the decided document, and false when nothing has been
// decided for the sandbox yet.
func (r *DockerSandboxRuntime) readRuntimeConfig(sandboxID string) (sandboxconfig.RuntimeConfig, bool, error) {
	data, err := os.ReadFile(r.runtimeConfigRecordPath(sandboxID))
	if errors.Is(err, fs.ErrNotExist) {
		return sandboxconfig.RuntimeConfig{}, false, nil
	}
	if err != nil {
		return sandboxconfig.RuntimeConfig{}, false, err
	}
	var record runtimeConfigRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return sandboxconfig.RuntimeConfig{}, false, fmt.Errorf("decode %s: %w", runtimeConfigRecordName, err)
	}
	return record.Document, true, nil
}

func (r *DockerSandboxRuntime) writeRuntimeConfig(sandboxID string, doc sandboxconfig.RuntimeConfig) error {
	data, err := json.MarshalIndent(runtimeConfigRecord{Document: withoutClientKey(doc)}, "", "  ")
	if err != nil {
		return err
	}
	path := r.runtimeConfigRecordPath(sandboxID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// The record names sentinels, never resolved values, but it is still the
	// pool's alone.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// decideRuntimeConfig settles the sandbox's document: the record as it stands
// with change applied to it, and the parts the pool owns outright — the idle
// timeout and the proxy material — as they are now. A document that differs
// from the record is a new revision; one that does not keeps the record's, so
// deciding again is free and delivering it again is a retry. The returned
// document carries the client key; the record does not. The caller holds
// runtimeConfigLock.
//
// Deciding issues the sandbox's proxy material and writes the record, so it is
// refused for a sandbox whose tree this pool does not hold or holds archived: a
// late caller must not put back what a delete or an archive just took away.
func (r *DockerSandboxRuntime) decideRuntimeConfig(sandboxID string, change func(*sandboxconfig.RuntimeConfig)) (sandboxconfig.RuntimeConfig, error) {
	switch {
	case !r.hostsSandbox(sandboxID):
		return sandboxconfig.RuntimeConfig{}, ErrNotFound
	case r.isArchived(sandboxID):
		return sandboxconfig.RuntimeConfig{}, ErrArchived
	}
	recorded, ok, err := r.readRuntimeConfig(sandboxID)
	if err != nil {
		return sandboxconfig.RuntimeConfig{}, err
	}
	// A copy, so a change that edits a map or a source in place is still a
	// change when compared with the record.
	next, err := cloneRuntimeConfig(recorded)
	if err != nil {
		return sandboxconfig.RuntimeConfig{}, err
	}
	if change != nil {
		change(&next)
	}
	next.Agent = sandboxconfig.RuntimeAgent{}
	if r.sandboxIdleTimeout > 0 {
		next.Agent.IdleTimeout = r.sandboxIdleTimeout.String()
	}
	material, err := proxyagent.EnsureSandboxMaterial(r.root, r.projectID, r.poolID, sandboxID)
	if err != nil {
		return sandboxconfig.RuntimeConfig{}, err
	}
	proxy := material.Proxy
	next.Proxy = &proxy
	next.Revision = recorded.Revision
	if !ok || !withoutClientKey(next).SameDocument(withoutClientKey(recorded)) {
		next.Revision = recorded.Revision + 1
		if err := r.writeRuntimeConfig(sandboxID, next); err != nil {
			return sandboxconfig.RuntimeConfig{}, fmt.Errorf("record runtime config: %w", err)
		}
	}
	return next, nil
}

// recordRuntimeConfig settles a change to the sandbox's document without
// delivering it, for a sandbox whose agent is not up to take it: the next
// start delivers what is recorded.
func (r *DockerSandboxRuntime) recordRuntimeConfig(sandboxID string, change func(*sandboxconfig.RuntimeConfig)) error {
	lock := r.runtimeConfigLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	_, err := r.decideRuntimeConfig(sandboxID, change)
	return err
}

// deliverRuntimeConfig settles the sandbox's document, change applied, and
// delivers it to the sandbox agent. A container from before the bootstrap named
// the pool's key is left as it is (sandboxLabelRuntimeConfig).
func (r *DockerSandboxRuntime) deliverRuntimeConfig(ctx context.Context, sandboxID string, change func(*sandboxconfig.RuntimeConfig)) error {
	lock := r.runtimeConfigLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	doc, err := r.decideRuntimeConfig(sandboxID, change)
	if err != nil {
		return err
	}
	return r.sendToTakingSandbox(ctx, sandboxID, doc)
}

// ConvergeRuntimeConfig delivers the sandbox's document when the revision its
// agent reports applying is not the one decided, deciding it again
// first so a changed idle timeout or renewed material reaches a sandbox that is
// already running. It is how a delivery that did not land is repaired: the
// pool converges on what the sandbox says it applied, never on having sent it.
//
// It steps aside for anything holding the sandbox's power lock — a create, a
// start, an archive, a delete — rather than waiting on it or racing it: the
// boot that work ends in delivers for itself, and the next poll looks again.
func (r *DockerSandboxRuntime) ConvergeRuntimeConfig(ctx context.Context, sandboxID string, applied int64) error {
	power := r.sandboxLock(sandboxID)
	if !power.TryLock() {
		return nil
	}
	defer power.Unlock()
	takes, err := r.takesRuntimeConfig(ctx, sandboxID)
	if err != nil || !takes {
		return err
	}
	lock := r.runtimeConfigLock(sandboxID)
	lock.Lock()
	defer lock.Unlock()
	doc, err := r.decideRuntimeConfig(sandboxID, nil)
	if err != nil {
		return err
	}
	// Only an equal revision is converged. A sandbox ahead of the record —
	// the record lost, or the sandbox moved here with its kept document — is
	// delivered to as well, so putRuntimeConfig can learn its revision, move
	// the record past it, and deliver what the pool decides now.
	if applied == doc.Revision {
		return nil
	}
	dial, err := r.SandboxDialer(ctx, sandboxID, SandboxAgentPort)
	if err != nil {
		return err
	}
	return r.putRuntimeConfig(ctx, dial, sandboxID, doc)
}

// sendToTakingSandbox delivers doc to a sandbox whose container takes runtime
// config. One that does not is from before the bootstrap named the pool's key:
// what was decided is recorded, and reaches it when its next start rebuilds it
// (retireContainerWithoutRuntimeConfig).
func (r *DockerSandboxRuntime) sendToTakingSandbox(ctx context.Context, sandboxID string, doc sandboxconfig.RuntimeConfig) error {
	takes, err := r.takesRuntimeConfig(ctx, sandboxID)
	if err != nil {
		return err
	}
	if !takes {
		slog.InfoContext(ctx, "sandbox container predates runtime-config delivery; the change reaches it when its next start rebuilds it", "sandboxId", sandboxID, "revision", doc.Revision)
		return nil
	}
	dial, err := r.SandboxDialer(ctx, sandboxID, SandboxAgentPort)
	if err != nil {
		return err
	}
	return r.putRuntimeConfig(ctx, dial, sandboxID, doc)
}

// takesRuntimeConfig reports whether the sandbox's container was built from a
// bootstrap that names this pool's key.
func (r *DockerSandboxRuntime) takesRuntimeConfig(ctx context.Context, sandboxID string) (bool, error) {
	sb, err := r.GetSandbox(ctx, sandboxID)
	if err != nil {
		return false, err
	}
	return r.containerTakesRuntimeConfig(ctx, sb.ID)
}

func (r *DockerSandboxRuntime) containerTakesRuntimeConfig(ctx context.Context, containerID string) (bool, error) {
	inspect, err := r.client.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return false, ErrNotFound
		}
		return false, err
	}
	if inspect.Container.Config == nil {
		return false, nil
	}
	return inspect.Container.Config.Labels[sandboxLabelRuntimeConfig] != "", nil
}

// putRuntimeConfig delivers doc. The sandbox answers with the document it holds:
// doc itself, or a newer one when the pool's record has fallen behind what the
// sandbox kept — a record lost with the pool's state, or a sandbox moved here
// with its kept document. A conflict is the same thing seen from the other
// side, a revision already spent on another document. Either way the record
// moves past the sandbox's and the document goes again, once.
func (r *DockerSandboxRuntime) putRuntimeConfig(ctx context.Context, dial Dialer, sandboxID string, doc sandboxconfig.RuntimeConfig) error {
	for attempt := 0; ; attempt++ {
		held, conflict, err := r.sendRuntimeConfig(ctx, dial, sandboxID, doc)
		if err != nil {
			return err
		}
		if !conflict && held.Revision <= doc.Revision {
			return nil
		}
		if attempt > 0 {
			return fmt.Errorf("sandbox %s holds runtime config revision %d, past the pool's %d, after one advance", sandboxID, held.Revision, doc.Revision)
		}
		doc.Revision = max(doc.Revision, held.Revision) + 1
		if err := r.writeRuntimeConfig(sandboxID, doc); err != nil {
			return fmt.Errorf("record runtime config: %w", err)
		}
	}
}

// sendRuntimeConfig makes one PUT. It returns the document the sandbox holds,
// or reports a conflict.
func (r *DockerSandboxRuntime) sendRuntimeConfig(ctx context.Context, dial Dialer, sandboxID string, doc sandboxconfig.RuntimeConfig) (sandboxconfig.RuntimeConfig, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, runtimeConfigCallTimeout)
	defer cancel()
	token, err := r.runtimeConfigToken(sandboxID)
	if err != nil {
		return sandboxconfig.RuntimeConfig{}, false, err
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return sandboxconfig.RuntimeConfig{}, false, err
	}
	target := HTTPURL(SandboxAgentPort, fmt.Sprintf("/api/projects/%s/sandboxes/%s/runtime-config", r.projectID, sandboxID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target.String(), bytes.NewReader(body))
	if err != nil {
		return sandboxconfig.RuntimeConfig{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	// The connection is no credential: the sandbox agent decides on this token
	// alone, whatever carried it (ADR 0126 §5).
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Transport: dial.Transport()}).Do(req)
	if err != nil {
		return sandboxconfig.RuntimeConfig{}, false, fmt.Errorf("deliver runtime config to sandbox %s: %w", sandboxID, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return sandboxconfig.RuntimeConfig{}, false, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var held sandboxconfig.RuntimeConfig
		if err := json.Unmarshal(data, &held); err != nil {
			return sandboxconfig.RuntimeConfig{}, false, fmt.Errorf("decode the runtime config sandbox %s holds: %w", sandboxID, err)
		}
		return held, false, nil
	case http.StatusConflict:
		return sandboxconfig.RuntimeConfig{}, true, nil
	case http.StatusNotFound:
		return sandboxconfig.RuntimeConfig{}, false, fmt.Errorf("sandbox %s: %w", sandboxID, ErrRuntimeConfigUnsupported)
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity:
		return sandboxconfig.RuntimeConfig{}, false, fmt.Errorf("sandbox %s: %w: status %d: %s", sandboxID, ErrRuntimeConfigRefused, resp.StatusCode, strings.TrimSpace(string(data)))
	default:
		return sandboxconfig.RuntimeConfig{}, false, fmt.Errorf("deliver runtime config to sandbox %s: status %d: %s", sandboxID, resp.StatusCode, strings.TrimSpace(string(data)))
	}
}

// runtimeConfigToken signs the token a delivery carries with the pool's
// identity key, which the sandbox's bootstrap names: the runtime-config scope
// and nothing else, for this sandbox and pool (ADR 26-10-08-127 §3).
func (r *DockerSandboxRuntime) runtimeConfigToken(sandboxID string) (string, error) {
	if len(r.identityKey) != ed25519.PrivateKeySize {
		return "", errors.New("the pool has no identity key to sign a runtime-config delivery with")
	}
	secretKey, err := paseto.NewV4AsymmetricSecretKeyFromEd25519(r.identityKey)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	token := paseto.NewToken()
	token.SetAudience(sandboxAgentAudience)
	token.SetIssuedAt(now)
	token.SetNotBefore(now.Add(-time.Minute))
	token.SetExpiration(now.Add(runtimeConfigTokenTTL))
	token.SetString("project_id", r.projectID)
	token.SetString("pool_id", r.poolID)
	token.SetString("sandbox_id", sandboxID)
	if err := token.Set("scopes", []string{sandboxconfig.RuntimeConfigScope}); err != nil {
		return "", err
	}
	return token.V4Sign(secretKey, nil), nil
}

// poolPublicKey is the public half of the pool's identity key, as the
// bootstrap carries it; empty when the runtime has no key.
func (r *DockerSandboxRuntime) poolPublicKey() string {
	if len(r.identityKey) != ed25519.PrivateKeySize {
		return ""
	}
	public, _ := r.identityKey.Public().(ed25519.PublicKey)
	return encodePublicKey(public)
}

// runtimeSources is the sandbox's sources as its document names them, each
// delivered when delivered says so.
func runtimeSources(sources []sandboxSource, delivered func(sandboxSource) bool) []sandboxconfig.RuntimeSource {
	out := make([]sandboxconfig.RuntimeSource, 0, len(sources))
	for _, source := range sources {
		out = append(out, sandboxconfig.RuntimeSource{
			Slug:      source.slug,
			Commit:    sourceBaseCommit(source.git),
			Delivered: delivered(source),
		})
	}
	return out
}

// logRuntimeConfigFailure records a delivery that did not land. The status
// poll converges on the revision the sandbox reports, so a delivery that fails
// here is retried there; an agent with no intake, or one that refuses the
// pool's delivery outright, is an error a caller has to see, since no retry
// makes that sandbox usable.
func logRuntimeConfigFailure(ctx context.Context, sandboxID string, err error) error {
	if errors.Is(err, ErrRuntimeConfigUnsupported) || errors.Is(err, ErrRuntimeConfigRefused) {
		return err
	}
	slog.WarnContext(ctx, "deliver sandbox runtime config; the status poll will retry", "sandboxId", sandboxID, "error", err)
	return nil
}

// cloneStringMap is a copy of in as a plain map, nil for an empty one, so a
// document never shares a map with the request it was decided from.
func cloneStringMap[M ~map[string]string](in M) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneRuntimeConfig(doc sandboxconfig.RuntimeConfig) (sandboxconfig.RuntimeConfig, error) {
	data, err := json.Marshal(doc)
	if err != nil {
		return sandboxconfig.RuntimeConfig{}, err
	}
	var out sandboxconfig.RuntimeConfig
	err = json.Unmarshal(data, &out)
	return out, err
}

// encodePublicKey is an Ed25519 public key as the sandbox agent reads one from
// its bootstrap: standard base64.
func encodePublicKey(key ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(key)
}

func withoutClientKey(doc sandboxconfig.RuntimeConfig) sandboxconfig.RuntimeConfig {
	if doc.Proxy == nil {
		return doc
	}
	proxy := *doc.Proxy
	proxy.ClientKey = ""
	doc.Proxy = &proxy
	return doc
}
