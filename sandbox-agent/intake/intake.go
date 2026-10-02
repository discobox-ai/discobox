// Package intake is the sandbox's side of the runtime-config document
// (sandboxconfig.RuntimeConfig, ADR 0126 §3): the one channel through which
// the pool tells a running sandbox what it is to be.
//
// Applying a document writes the same local files the agent already reads —
// sandbox.json's idle timeout, the secrets file, the proxy material and the
// source-readiness marker — so `config`, `secretswatch` and `sourcesready` are
// unchanged by it. What changes is who writes them.
//
// A delivery is all or nothing. Every file is rendered and staged beside its
// target before any target changes, the targets are then replaced in an order
// that clears the readiness gate last, and a replacement that fails puts back
// what every earlier one replaced. The document is kept, so a restart applies
// it again without waiting on the pool.
package intake

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/discobox-ai/discobox/sandboxconfig"
)

// Layout is where a document's files land.
type Layout struct {
	// ConfigDir holds sandbox.json and the readiness marker.
	ConfigDir string
	// ProxyDir holds the proxy material and the bridge configs that name it.
	ProxyDir string
	// SecretsPath is the resolved-secrets file secretswatch watches.
	SecretsPath string
	// StatePath is where the last applied document is kept. It holds the
	// sandbox's client key, so it is the agent's alone (0600).
	StatePath string
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
	}
}

// ErrInvalid wraps a document that fails validation.
var ErrInvalid = errors.New("invalid runtime config")

// ErrConflict is a document that differs from the one already applied under
// the same revision.
var ErrConflict = errors.New("a different runtime config was already applied at this revision")

// Intake applies runtime-config documents and remembers the last one.
type Intake struct {
	layout Layout

	mu      sync.Mutex
	applied *sandboxconfig.RuntimeConfig
}

// Open returns the intake for layout and applies the document it last kept,
// if any, so the files a restart lost (/run is a tmpfs) are back before
// anything reads them. The intake is usable even when that fails — the error
// says why nothing was restored, and the pool's next delivery repairs it.
func Open(layout Layout) (*Intake, error) {
	in := &Intake{layout: layout}
	data, err := os.ReadFile(layout.StatePath)
	if errors.Is(err, fs.ErrNotExist) {
		return in, nil
	}
	if err != nil {
		return in, fmt.Errorf("read kept runtime config: %w", err)
	}
	var kept sandboxconfig.RuntimeConfig
	if err := json.Unmarshal(data, &kept); err != nil {
		return in, fmt.Errorf("decode kept runtime config %s: %w", layout.StatePath, err)
	}
	// The kept document carries no client key (see plan); the key is the one
	// already in the proxy directory, which is its only copy on disk.
	if kept.Proxy != nil && kept.Proxy.ClientKey == "" {
		key, err := os.ReadFile(filepath.Join(layout.ProxyDir, clientKeyFile))
		if err != nil {
			return in, fmt.Errorf("read the client key the kept runtime config names: %w", err)
		}
		kept.Proxy.ClientKey = string(key)
	}
	if err := kept.Validate(); err != nil {
		return in, fmt.Errorf("kept runtime config %s: %w", layout.StatePath, err)
	}
	// The state file already holds this document; only its files are put back.
	if err := in.commit(kept, false); err != nil {
		return in, fmt.Errorf("restore runtime config revision %d: %w", kept.Revision, err)
	}
	in.applied = &kept
	return in, nil
}

// Applied returns the last document applied, and false when there is none.
func (in *Intake) Applied() (sandboxconfig.RuntimeConfig, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.applied == nil {
		return sandboxconfig.RuntimeConfig{}, false
	}
	return *in.applied, true
}

// Revision is the revision applied, zero when none has been.
func (in *Intake) Revision() int64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.applied == nil {
		return 0
	}
	return in.applied.Revision
}

// Apply converges the sandbox on doc and returns the document it now holds.
//
// A revision older than the one applied is ignored and the held document
// returned, so a delayed delivery cannot roll the sandbox back. The same
// revision is a retry when the document matches, and ErrConflict when it does
// not. A document that fails validation, or whose files cannot all be written,
// changes nothing.
func (in *Intake) Apply(doc sandboxconfig.RuntimeConfig) (sandboxconfig.RuntimeConfig, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if err := doc.Validate(); err != nil {
		return sandboxconfig.RuntimeConfig{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if held := in.applied; held != nil {
		switch {
		case doc.Revision < held.Revision:
			return *held, nil
		case doc.Revision == held.Revision && doc.SameDocument(*held):
			return *held, nil
		case doc.Revision == held.Revision:
			return sandboxconfig.RuntimeConfig{}, fmt.Errorf("%w (revision %d)", ErrConflict, doc.Revision)
		}
	}
	if err := in.commit(doc, true); err != nil {
		return sandboxconfig.RuntimeConfig{}, err
	}
	in.applied = &doc
	return doc, nil
}

// commit writes doc's files, and the state file when keep is set, as one
// change.
func (in *Intake) commit(doc sandboxconfig.RuntimeConfig, keep bool) error {
	ops, err := in.plan(doc, keep)
	if err != nil {
		return err
	}
	return run(ops)
}

// plan renders every file doc implies, in the order they are to be replaced.
//
// The readiness marker brackets the rest: a document that withholds readiness
// removes the marker before anything else changes, and one that grants it
// writes the marker after everything else is in place. Either way a gate is
// never open over files from two documents. The state file is last, so a kept
// document is never newer than the files it describes.
func (in *Intake) plan(doc sandboxconfig.RuntimeConfig, keep bool) ([]op, error) {
	ready := filepath.Join(in.layout.ConfigDir, sandboxconfig.SourcesReadyFileName)
	var ops []op
	delivered := doc.SourcesDelivered()
	if !delivered {
		ops = append(ops, op{path: ready, remove: true})
	}
	proxyOps, err := proxyFiles(in.layout.ProxyDir, doc.Proxy)
	if err != nil {
		return nil, err
	}
	ops = append(ops, proxyOps...)
	secrets, err := secretsFile(in.layout.SecretsPath, doc.SecretEnv)
	if err != nil {
		return nil, err
	}
	ops = append(ops, secrets)
	manifest, changed, err := manifestFile(filepath.Join(in.layout.ConfigDir, manifestName), doc.Agent)
	if err != nil {
		return nil, err
	}
	if changed {
		ops = append(ops, manifest)
	}
	if delivered {
		//nolint:gosec // a public runtime signal read by the sandbox, like sandbox.json beside it.
		ops = append(ops, op{path: ready, data: []byte{}, mode: 0o644})
	}
	if keep {
		// /var/lib/discobox is a data volume, which an export carries, so the
		// kept document leaves the client key out: client.key in the proxy
		// directory stays the one copy, and a restore reads it from there.
		state, err := json.MarshalIndent(WithoutClientKey(doc), "", "  ")
		if err != nil {
			return nil, err
		}
		ops = append(ops, op{path: in.layout.StatePath, data: state, mode: 0o600})
	}
	return ops, nil
}

// manifestName is the sandbox manifest inside ConfigDir.
const manifestName = "sandbox.json"

// manifestFile is sandbox.json with the agent settings applied, and whether
// that changes it. Only the keys the document owns are touched: the rest of the
// file is the create-time placement and is carried through as it was, numbers
// included. Like the pool's own rewrite, it sets the value both in the
// effective config and in its runtime provenance, and a manifest that does not
// exist is left alone — the sandbox it belongs to cannot boot anyway.
func manifestFile(path string, agent sandboxconfig.RuntimeAgent) (op, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return op{}, false, nil
	}
	if err != nil {
		return op{}, false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var manifest map[string]any
	if err := decoder.Decode(&manifest); err != nil {
		return op{}, false, fmt.Errorf("decode %s: %w", path, err)
	}
	changed := setIdleTimeout(manifest, agent.IdleTimeout)
	if provenance, ok := manifest["_provenance"].(map[string]any); ok {
		if runtime, ok := provenance["runtime"].(map[string]any); ok {
			changed = setIdleTimeout(runtime, agent.IdleTimeout) || changed
		}
	}
	if !changed {
		return op{}, false, nil
	}
	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return op{}, false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return op{}, false, err
	}
	return op{path: path, data: out, mode: info.Mode().Perm()}, true, nil
}

// setIdleTimeout sets agentRuntime.idleTimeout in the object holding an
// agentRuntime, removing it when want is empty, and reports whether it changed.
func setIdleTimeout(holder map[string]any, want string) bool {
	agentRuntime, ok := holder["agentRuntime"].(map[string]any)
	if !ok {
		if want == "" {
			return false
		}
		agentRuntime = map[string]any{}
		holder["agentRuntime"] = agentRuntime
	}
	current, _ := agentRuntime["idleTimeout"].(string)
	if current == want {
		return false
	}
	if want == "" {
		delete(agentRuntime, "idleTimeout")
	} else {
		agentRuntime["idleTimeout"] = want
	}
	return true
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
	CredentialsURL   string `json:"credentialsUrl,omitempty"`
	DNSServer        string `json:"dnsServer,omitempty"`
	DNSListenAddress string `json:"dnsListenAddress,omitempty"`
	MTLSCAPath       string `json:"mtlsCaPath"`
	ClientCertPath   string `json:"clientCertPath"`
	ClientKeyPath    string `json:"clientKeyPath"`
}

// proxyFiles renders the proxy material into dir. A bridge config names the
// keypair and CA by where this sandbox keeps them, which only it knows.
func proxyFiles(dir string, proxy *sandboxconfig.RuntimeProxy) ([]op, error) {
	files := map[string]op{}
	if proxy != nil {
		//nolint:gosec // CA certificates and the client certificate are public.
		files[mtlsCAFile] = op{data: []byte(proxy.MTLSCA), mode: 0o644}
		//nolint:gosec // as above.
		files[mitmCAFile] = op{data: []byte(proxy.MITMCA), mode: 0o644}
		//nolint:gosec // as above.
		files[clientCertFile] = op{data: []byte(proxy.ClientCert), mode: 0o644}
		files[clientKeyFile] = op{data: []byte(proxy.ClientKey), mode: 0o600}
		for name, bridge := range map[string]*sandboxconfig.RuntimeBridge{
			egressBridgeFile:       proxy.Egress,
			nestedDockerBridgeFile: proxy.NestedDocker,
			buildKitBridgeFile:     proxy.BuildKit,
		} {
			if bridge == nil {
				continue
			}
			data, err := json.MarshalIndent(bridgeFile{
				ListenAddress:    bridge.ListenAddress,
				UpstreamURL:      bridge.UpstreamURL,
				CredentialsURL:   bridge.CredentialsURL,
				DNSServer:        bridge.DNSServer,
				DNSListenAddress: bridge.DNSListenAddress,
				MTLSCAPath:       filepath.Join(dir, mtlsCAFile),
				ClientCertPath:   filepath.Join(dir, clientCertFile),
				ClientKeyPath:    filepath.Join(dir, clientKeyFile),
			}, "", "  ")
			if err != nil {
				return nil, err
			}
			files[name] = op{data: data, mode: 0o600}
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
