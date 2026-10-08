// Package intake is the sandbox's side of the runtime-config document
// (sandboxconfig.RuntimeConfig, ADR 0126 §3): the one channel through which
// the pool tells a running sandbox what it is to be.
//
// The document is the dynamic half of what the sandbox is; the static half is
// the bootstrap, sandbox.json, which this package never writes
// (ADR 26-10-08-127 §1). Applying a document writes the local files the
// agent's readers already watch — the secrets file, the proxy material and the
// bridge configs rendered from it, and the source-readiness marker — so
// `secretswatch` and `sourcesready` are unchanged by it. What it carries for
// the agent itself, the idle timeout, the caller applies from Applied.
//
// A delivery is all or nothing. Every file is rendered and staged beside its
// target before any target changes, the targets are then replaced in order,
// and a replacement that fails puts back what every earlier one replaced. The
// units that read what changed are started before readiness is published, so
// the gate never opens over a hop that is not up. The document is kept, so a
// restart applies it again without waiting on the pool.
package intake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/discobox-ai/discobox/platform"
	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/sandboxpath"
)

// Layout is where a document's files land.
type Layout struct {
	// ConfigDir holds the readiness marker, beside sandbox.json.
	ConfigDir string
	// ProxyDir holds the proxy material and the bridge configs that name it.
	ProxyDir string
	// SecretsPath is the resolved-secrets file secretswatch watches.
	SecretsPath string
	// StatePath is where the last applied document is kept. It holds the
	// sandbox's client key, so it is the agent's alone (0600).
	StatePath string
	// Paths is this sandbox's platform's path rules, which a document's
	// source targets are judged by. The zero value judges POSIX paths.
	Paths sandboxpath.Paths
}

// DefaultLayout is the paths the sandbox's readers use. The state file sits
// beside the agent's database on /var/lib/discobox, a data volume, so it
// survives the restarts that empty /run.
func DefaultLayout() Layout {
	//nolint:gosec // G101: file paths, not credentials.
	return Layout{
		ConfigDir:   sandboxconfig.SandboxConfigDir,
		ProxyDir:    filepath.Join(sandboxconfig.SandboxConfigDir, "proxy"),
		SecretsPath: "/run/discobox/secrets/secrets.json",
		StatePath:   "/var/lib/discobox/runtime-config.json",
		// The agent runs inside the sandbox, so its own platform is the
		// sandbox's.
		Paths: sandboxpath.For(platform.Current()),
	}
}

// ErrInvalid wraps a document that fails validation.
var ErrInvalid = errors.New("invalid runtime config")

// ErrConflict is a document that differs from the one already applied under
// the same revision.
var ErrConflict = errors.New("a different runtime config was already applied at this revision")

// Owner is the sandbox a kept document was applied for, as sandbox.json names
// it.
type Owner struct {
	ProjectID string `json:"projectId"`
	SandboxID string `json:"sandboxId"`
	PoolID    string `json:"poolId"`
}

// keptDocument is the state file: the document, and whose it is.
//
// The owner is there because the file travels. /var/lib/discobox is a data
// volume an export carries, so a sandbox brought up again elsewhere starts
// with the document its old pool sent — and that document's revision would
// then outrank everything its new pool sends, whose revisions owe nothing to
// the old pool's. A document is restored only into the sandbox and pool it was
// applied for.
type keptDocument struct {
	Owner    Owner                       `json:"owner"`
	Document sandboxconfig.RuntimeConfig `json:"document"`
}

// Activator starts what reads the files a delivery changed, named by their
// paths. It is called after they are in place and before readiness is
// published; it reports nothing back, because the files are already the
// document's and a unit that will not start is the unit's to say so.
type Activator func(ctx context.Context, changed []string)

// Config is what an intake applies documents for.
type Config struct {
	Layout Layout
	// Owner is the sandbox the documents are for.
	Owner Owner
	// Pool is where the sandbox's pool serves it, from the bootstrap. The
	// bridges are rendered from it; nil renders none.
	Pool *sandboxconfig.PoolEndpoints
	// Activate is called with the files a delivery changed; nil starts nothing.
	Activate Activator
}

// Intake applies runtime-config documents and remembers the last one.
type Intake struct {
	layout   Layout
	owner    Owner
	pool     *sandboxconfig.PoolEndpoints
	activate Activator

	// mu serializes deliveries, which hold it while they write files and start
	// units. Readers do not take it: applied is read on every status poll, which
	// must not wait out a delivery restarting units.
	mu      sync.Mutex
	applied atomic.Pointer[sandboxconfig.RuntimeConfig]
}

// Open returns the intake for cfg.Owner's sandbox, and applies the document it
// last kept, if that was the owner's, so the files a restart lost (/run is a
// tmpfs) are back before anything reads them. The intake is usable even when
// that fails — the error says why nothing was restored, and the pool's next
// delivery repairs it. A document kept for another sandbox or pool is not
// restored and orders nothing; the first delivery replaces it.
func Open(ctx context.Context, cfg Config) (*Intake, error) {
	in := &Intake{layout: cfg.Layout, owner: cfg.Owner, pool: cfg.Pool, activate: cfg.Activate}
	layout, owner := cfg.Layout, cfg.Owner
	data, err := os.ReadFile(layout.StatePath)
	if errors.Is(err, fs.ErrNotExist) {
		return in, nil
	}
	if err != nil {
		return in, fmt.Errorf("read kept runtime config: %w", err)
	}
	var file keptDocument
	if err := json.Unmarshal(data, &file); err != nil {
		return in, fmt.Errorf("decode kept runtime config %s: %w", layout.StatePath, err)
	}
	if file.Owner != owner {
		return in, fmt.Errorf("kept runtime config %s was applied for %+v, not this sandbox (%+v); not restored", layout.StatePath, file.Owner, owner)
	}
	kept := file.Document
	// The kept document carries no client key (see plan); the key is the one
	// already in the proxy directory, which is its only copy on disk.
	if kept.Proxy != nil && kept.Proxy.ClientKey == "" {
		key, err := os.ReadFile(filepath.Join(layout.ProxyDir, clientKeyFile))
		if err != nil {
			return in, fmt.Errorf("read the client key the kept runtime config names: %w", err)
		}
		kept.Proxy.ClientKey = string(key)
	}
	if err := kept.Validate(layout.Paths); err != nil {
		return in, fmt.Errorf("kept runtime config %s: %w", layout.StatePath, err)
	}
	// The state file already holds this document; only its files are put back.
	if err := in.commit(ctx, kept, false); err != nil {
		return in, fmt.Errorf("restore runtime config revision %d: %w", kept.Revision, err)
	}
	in.applied.Store(&kept)
	return in, nil
}

// Applied returns the last document applied, and false when there is none.
func (in *Intake) Applied() (sandboxconfig.RuntimeConfig, bool) {
	held := in.applied.Load()
	if held == nil {
		return sandboxconfig.RuntimeConfig{}, false
	}
	return *held, true
}

// Revision is the revision applied, zero when none has been.
func (in *Intake) Revision() int64 {
	held := in.applied.Load()
	if held == nil {
		return 0
	}
	return held.Revision
}

// Apply converges the sandbox on doc and returns the document it now holds.
//
// A revision older than the one applied is ignored and the held document
// returned, so a delayed delivery cannot roll the sandbox back. The same
// revision is a retry when the document matches, and ErrConflict when it does
// not. A document that fails validation, or whose files cannot all be written,
// changes nothing.
func (in *Intake) Apply(ctx context.Context, doc sandboxconfig.RuntimeConfig) (sandboxconfig.RuntimeConfig, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	// Ordering comes first: only a document newer than the held one can
	// change anything, so only that one is worth validating. An out-of-date
	// delivery is ignored whatever it says, and anything but the held
	// document under the held revision is a conflict, malformed or not.
	if held := in.applied.Load(); held != nil {
		switch {
		case doc.Revision < held.Revision:
			return *held, nil
		case doc.Revision == held.Revision && doc.SameDocument(*held):
			return *held, nil
		case doc.Revision == held.Revision:
			return sandboxconfig.RuntimeConfig{}, fmt.Errorf("%w (revision %d)", ErrConflict, doc.Revision)
		}
	}
	if err := doc.Validate(in.layout.Paths); err != nil {
		return sandboxconfig.RuntimeConfig{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := in.commit(ctx, doc, true); err != nil {
		return sandboxconfig.RuntimeConfig{}, err
	}
	in.applied.Store(&doc)
	return doc, nil
}

// commit writes doc's files, and the state file when keep is set, as one
// change, then starts what reads the files that changed, then publishes
// readiness when doc grants it.
func (in *Intake) commit(ctx context.Context, doc sandboxconfig.RuntimeConfig, keep bool) error {
	ops, gate, err := in.plan(doc, keep)
	if err != nil {
		return err
	}
	if gate != nil {
		ops = append(ops, *gate)
	}
	activate := func(done []op) {
		if in.activate == nil {
			return
		}
		if changed := changedPaths(done); len(changed) > 0 {
			in.activate(ctx, changed)
		}
	}
	return run(ops, gate != nil, activate)
}

// plan renders every file doc implies, in the order they are to be replaced,
// and the readiness marker that follows them when doc grants it.
//
// The readiness marker brackets the rest. It is removed before anything else
// changes, whatever the document says — a marker left up from the previous
// document while this one's files go in would be a gate open over files from
// two documents. A document that grants readiness writes it again after
// everything else is in place, the state file included, since a failed rename
// there rolls the rest back, and after the units reading the new files have
// been started, so that a waiter never runs ahead of the hop it needs. The
// state file comes after every file it describes, so a kept document is never
// newer than they are.
func (in *Intake) plan(doc sandboxconfig.RuntimeConfig, keep bool) ([]op, *op, error) {
	ready := filepath.Join(in.layout.ConfigDir, sandboxconfig.SourcesReadyFileName)
	var ops []op
	ops = append(ops, op{path: ready, remove: true})
	proxyOps, err := proxyFiles(in.layout.ProxyDir, in.pool, doc.Proxy)
	if err != nil {
		return nil, nil, err
	}
	ops = append(ops, proxyOps...)
	secrets, err := secretsFile(in.layout.SecretsPath, doc.SecretEnv)
	if err != nil {
		return nil, nil, err
	}
	ops = append(ops, secrets)
	if keep {
		// /var/lib/discobox is a data volume, which an export carries, so the
		// kept document leaves the client key out: client.key in the proxy
		// directory stays the one copy, and a restore reads it from there.
		state, err := json.MarshalIndent(keptDocument{Owner: in.owner, Document: WithoutClientKey(doc)}, "", "  ")
		if err != nil {
			return nil, nil, err
		}
		ops = append(ops, op{path: in.layout.StatePath, data: state, mode: 0o600})
	}
	if !doc.SourcesDelivered() {
		return ops, nil, nil
	}
	//nolint:gosec // a public runtime signal read by the sandbox, like sandbox.json beside it.
	return ops, &op{path: ready, data: []byte{}, mode: 0o644}, nil
}

// changedPaths are the targets of done whose contents a replacement changed,
// sorted. The readiness marker is not among them: nothing is started for it.
func changedPaths(done []op) []string {
	var out []string
	for _, o := range done {
		if filepath.Base(o.path) == sandboxconfig.SourcesReadyFileName {
			continue
		}
		switch {
		case o.remove && o.prior != nil:
		case !o.remove && (o.prior == nil || *o.prior != o.mode || !bytes.Equal(o.held, o.data)):
		default:
			continue
		}
		out = append(out, o.path)
	}
	sort.Strings(out)
	return out
}

// secretsFile is the resolved-secrets file: env name to sentinel, readable by
// the agent alone. An empty map is written rather than the file removed, so a
// sandbox whose last secret was unbound reads "none" rather than "not yet".
func secretsFile(path string, env map[string]string) (op, error) {
	if env == nil {
		env = map[string]string{}
	}
	data, err := json.Marshal(env)
	if err != nil {
		return op{}, err
	}
	return op{path: path, data: data, mode: 0o600, dirMode: 0o700}, nil
}

// Proxy material file names, the ones the sandbox's units and readers name.
const (
	mtlsCAFile             = "mtls-ca.crt"
	mitmCAFile             = "mitm-ca.crt"
	clientCertFile         = "client.crt"
	clientKeyFile          = "client.key"
	egressBridgeFile       = "bridge.json"
	nestedDockerBridgeFile = "bridge-docker.json"
	buildKitBridgeFile     = "bridge-buildkit.json"
	registryNamespaceFile  = "registry-namespace"
)

// proxyFileNames is every file the proxy directory may hold, so that what a
// document leaves out is removed rather than left over from an older one.
var proxyFileNames = []string{
	mtlsCAFile, mitmCAFile, clientCertFile, clientKeyFile,
	egressBridgeFile, nestedDockerBridgeFile, buildKitBridgeFile, registryNamespaceFile,
}

// bridgeFile is the bridge config as the forwarders, the DNS stub and the
// credentials relay decode it.
type bridgeFile struct {
	ListenAddress    string `json:"listenAddress"`
	UpstreamURL      string `json:"workerProxyUrl"`
	ServerName       string `json:"serverName,omitempty"`
	CredentialsURL   string `json:"credentialsUrl,omitempty"`
	DNSServer        string `json:"dnsServer,omitempty"`
	DNSListenAddress string `json:"dnsListenAddress,omitempty"`
	MTLSCAPath       string `json:"mtlsCaPath"`
	ClientCertPath   string `json:"clientCertPath"`
	ClientKeyPath    string `json:"clientKeyPath"`
}

// proxyFiles renders the proxy material into dir, and each bridge config from
// its three parts: where the pool serves it (pool, from the bootstrap), where
// this sandbox listens for it (sandboxconfig's constants), and the credential
// the document delivers, named by where this sandbox keeps it. A bridge exists
// only where all three do; the nested-Docker one names no listener, which the
// sandbox discovers when its dockerd makes one (ADR 26-10-08-127 §4).
func proxyFiles(dir string, pool *sandboxconfig.PoolEndpoints, proxy *sandboxconfig.RuntimeProxy) ([]op, error) {
	files := map[string]op{}
	if proxy != nil {
		//nolint:gosec // CA certificates and the client certificate are public.
		files[mtlsCAFile] = op{data: []byte(proxy.MTLSCA), mode: 0o644}
		//nolint:gosec // as above.
		files[mitmCAFile] = op{data: []byte(proxy.MITMCA), mode: 0o644}
		//nolint:gosec // as above.
		files[clientCertFile] = op{data: []byte(proxy.ClientCert), mode: 0o644}
		files[clientKeyFile] = op{data: []byte(proxy.ClientKey), mode: 0o600}
		if pool != nil && pool.Proxy != "" {
			egress := bridgeFile{
				ListenAddress:  sandboxconfig.SandboxEgressListenAddress,
				UpstreamURL:    pool.Proxy,
				CredentialsURL: pool.Credentials,
			}
			if pool.DNS != "" {
				egress.DNSServer = pool.DNS
				egress.DNSListenAddress = sandboxconfig.SandboxDNSListenAddress
			}
			bridges := map[string]bridgeFile{
				egressBridgeFile:       egress,
				nestedDockerBridgeFile: {UpstreamURL: pool.Proxy},
			}
			if pool.BuildKit != "" {
				bridges[buildKitBridgeFile] = bridgeFile{
					ListenAddress: sandboxconfig.SandboxBuildKitListenAddress,
					UpstreamURL:   pool.BuildKit,
				}
			}
			for name, bridge := range bridges {
				bridge.ServerName = pool.ServerName
				bridge.MTLSCAPath = filepath.Join(dir, mtlsCAFile)
				bridge.ClientCertPath = filepath.Join(dir, clientCertFile)
				bridge.ClientKeyPath = filepath.Join(dir, clientKeyFile)
				data, err := json.MarshalIndent(bridge, "", "  ")
				if err != nil {
					return nil, err
				}
				files[name] = op{data: data, mode: 0o600}
			}
		}
		if proxy.RegistryNamespace != "" {
			//nolint:gosec // readable inside the sandbox by design, as the pool staged it.
			files[registryNamespaceFile] = op{data: []byte(proxy.RegistryNamespace + "\n"), mode: 0o644}
		}
	}
	ops := make([]op, 0, len(proxyFileNames))
	for _, name := range proxyFileNames {
		file, ok := files[name]
		if !ok {
			file = op{remove: true}
		}
		file.path = filepath.Join(dir, name)
		ops = append(ops, file)
	}
	return ops, nil
}

// WithoutClientKey is doc with the sandbox's client private key removed: what
// is kept on disk beside it and what is answered to a caller, neither of which
// needs the key, which only the bridges read and only from client.key.
func WithoutClientKey(doc sandboxconfig.RuntimeConfig) sandboxconfig.RuntimeConfig {
	if doc.Proxy == nil {
		return doc
	}
	proxy := *doc.Proxy
	proxy.ClientKey = ""
	doc.Proxy = &proxy
	return doc
}
