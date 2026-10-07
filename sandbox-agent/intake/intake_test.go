package intake

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandboxconfig"
)

var testOwner = Owner{ProjectID: "prj_1", SandboxID: "sbx_1", PoolID: "pool_1"}

func testLayout(t *testing.T) Layout {
	t.Helper()
	root := t.TempDir()
	return Layout{
		ConfigDir:   filepath.Join(root, "etc"),
		ProxyDir:    filepath.Join(root, "etc", "proxy"),
		SecretsPath: filepath.Join(root, "run", "secrets", "secrets.json"),
		StatePath:   filepath.Join(root, "var", "runtime-config.json"),
	}
}

// testPKI is a CA and a client keypair it signed, as PEM.
type testPKI struct {
	ca, cert, key string
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "sbx_1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, caTemplate, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	return testPKI{
		ca:   string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		cert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER})),
		key:  string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
	}
}

func testDocument(t *testing.T, revision int64) sandboxconfig.RuntimeConfig {
	t.Helper()
	pki := newTestPKI(t)
	return sandboxconfig.RuntimeConfig{
		Revision:  revision,
		Agent:     sandboxconfig.RuntimeAgent{IdleTimeout: "45m0s"},
		SecretEnv: map[string]string{"GH_TOKEN": "discobox-sentinel-1"},
		Proxy: &sandboxconfig.RuntimeProxy{
			MTLSCA:     pki.ca,
			MITMCA:     pki.ca,
			ClientCert: pki.cert,
			ClientKey:  pki.key,
			//nolint:gosec // G101: addresses, not credentials.
			Egress: &sandboxconfig.RuntimeBridge{
				ListenAddress:    "127.0.0.1:17008",
				UpstreamURL:      "https://pool:17443",
				CredentialsURL:   "https://pool:17444",
				DNSServer:        "pool:853",
				DNSListenAddress: "169.254.0.53:53",
			},
			NestedDocker:      &sandboxconfig.RuntimeBridge{UpstreamURL: "https://pool:17443"},
			BuildKit:          &sandboxconfig.RuntimeBridge{ListenAddress: "127.0.0.1:17082", UpstreamURL: "https://pool:17445"},
			RegistryNamespace: "ns-0123abcd",
		},
		Sources: []sandboxconfig.RuntimeSource{{
			Slug:      "primary",
			OriginURL: "https://pool/origins/primary",
			Commit:    "0123456789abcdef0123456789abcdef01234567",
			Delivered: true,
		}},
	}
}

// writeManifest places the create-time sandbox.json the document's agent
// settings are applied into.
func writeManifest(t *testing.T, layout Layout, idleTimeout string) {
	t.Helper()
	manifest := map[string]any{
		"apiVersion":   sandboxconfig.APIVersion,
		"sandboxId":    "sbx_1",
		"user":         map[string]any{"uid": 4294967294},
		"agentRuntime": map[string]any{"listenAddress": ":3003", "idleTimeout": idleTimeout},
		"_provenance": map[string]any{
			"runtime": map[string]any{"agentRuntime": map[string]any{"idleTimeout": idleTimeout}},
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.ConfigDir, manifestName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readManifestIdleTimeouts(t *testing.T, layout Layout) (effective, provenance string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(layout.ConfigDir, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		User         struct{ UID json.Number } `json:"user"`
		AgentRuntime struct {
			IdleTimeout string `json:"idleTimeout"`
		} `json:"agentRuntime"`
		Provenance struct {
			Runtime struct {
				AgentRuntime struct {
					IdleTimeout string `json:"idleTimeout"`
				} `json:"agentRuntime"`
			} `json:"runtime"`
		} `json:"_provenance"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.User.UID != "4294967294" {
		t.Fatalf("manifest user uid = %q, want the create-time value carried through", manifest.User.UID)
	}
	return manifest.AgentRuntime.IdleTimeout, manifest.Provenance.Runtime.AgentRuntime.IdleTimeout
}

// assertMode checks a file's POSIX permission bits. Windows has none — every
// file reports 0666 or 0444 — so there is nothing to assert there; the sandbox
// these modes protect is Linux or macOS.
func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %#o, want %#o", path, got, want)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// snapshot is every file under the layout's roots, for asserting that a
// failed delivery changed nothing.
func snapshot(t *testing.T, layout Layout) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := filepath.Dir(layout.ConfigDir)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // G122: a test's own temp directory.
		if err != nil {
			return err
		}
		out[path] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestApplyWritesTheFilesTheReadersRead(t *testing.T) {
	layout := testLayout(t)
	writeManifest(t, layout, "30m0s")
	in, err := Open(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	doc := testDocument(t, 1)
	applied, err := in.Apply(doc)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Revision != 1 || in.Revision() != 1 {
		t.Fatalf("applied revision = %d / %d, want 1", applied.Revision, in.Revision())
	}

	var secrets map[string]string
	if err := json.Unmarshal([]byte(readFile(t, layout.SecretsPath)), &secrets); err != nil {
		t.Fatal(err)
	}
	if secrets["GH_TOKEN"] != "discobox-sentinel-1" || len(secrets) != 1 {
		t.Fatalf("secrets = %v", secrets)
	}
	assertMode(t, layout.SecretsPath, 0o600)
	assertMode(t, filepath.Dir(layout.SecretsPath), 0o700)

	if effective, provenance := readManifestIdleTimeouts(t, layout); effective != "45m0s" || provenance != "45m0s" {
		t.Fatalf("manifest idle timeout = %q / %q, want 45m0s in both", effective, provenance)
	}

	if got := readFile(t, filepath.Join(layout.ProxyDir, clientKeyFile)); got != doc.Proxy.ClientKey {
		t.Fatalf("client key not written")
	}
	assertMode(t, filepath.Join(layout.ProxyDir, clientKeyFile), 0o600)
	if got := readFile(t, filepath.Join(layout.ProxyDir, registryNamespaceFile)); got != "ns-0123abcd\n" {
		t.Fatalf("registry namespace = %q", got)
	}
	var egress bridgeFile
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(layout.ProxyDir, egressBridgeFile))), &egress); err != nil {
		t.Fatal(err)
	}
	if egress.UpstreamURL != "https://pool:17443" || egress.DNSListenAddress != "169.254.0.53:53" ||
		egress.ClientKeyPath != filepath.Join(layout.ProxyDir, clientKeyFile) ||
		egress.MTLSCAPath != filepath.Join(layout.ProxyDir, mtlsCAFile) {
		t.Fatalf("egress bridge = %+v", egress)
	}
	for _, name := range []string{nestedDockerBridgeFile, buildKitBridgeFile, mtlsCAFile, mitmCAFile, clientCertFile} {
		if !exists(filepath.Join(layout.ProxyDir, name)) {
			t.Fatalf("%s not written", name)
		}
	}

	if !exists(filepath.Join(layout.ConfigDir, sandboxconfig.SourcesReadyFileName)) {
		t.Fatal("a delivered document did not publish readiness")
	}
	kept, ok := in.Applied()
	if !ok || !kept.SameDocument(doc) {
		t.Fatalf("Applied() = %+v, %v", kept, ok)
	}
	for _, path := range []string{layout.SecretsPath, layout.StatePath} {
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".runtime-config-") {
				t.Fatalf("staged file %s left behind", entry.Name())
			}
		}
	}
}

func TestApplyIgnoresAnOlderRevision(t *testing.T) {
	layout := testLayout(t)
	in, err := Open(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	newer := testDocument(t, 5)
	if _, err := in.Apply(newer); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, layout)

	older := testDocument(t, 4)
	older.SecretEnv = map[string]string{"OLD": "discobox-sentinel-old"}
	held, err := in.Apply(older)
	if err != nil {
		t.Fatalf("an older revision is ignored, not refused: %v", err)
	}
	if held.Revision != 5 || in.Revision() != 5 {
		t.Fatalf("held revision = %d / %d, want 5", held.Revision, in.Revision())
	}
	if after := snapshot(t, layout); !equalSnapshots(before, after) {
		t.Fatal("an older revision changed the sandbox's files")
	}
}

func TestApplySameRevisionIsARetryOrAConflict(t *testing.T) {
	layout := testLayout(t)
	in, err := Open(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	doc := testDocument(t, 2)
	if _, err := in.Apply(doc); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Apply(doc); err != nil {
		t.Fatalf("re-delivering the applied document: %v", err)
	}
	different := doc
	different.SecretEnv = map[string]string{"OTHER": "discobox-sentinel-2"}
	if _, err := in.Apply(different); !errors.Is(err, ErrConflict) {
		t.Fatalf("a different document under the applied revision: err = %v, want ErrConflict", err)
	}
}

func TestApplyThatFailsLeavesThePreviousStateIntact(t *testing.T) {
	cases := map[string]func(t *testing.T, layout Layout, doc *sandboxconfig.RuntimeConfig){
		"invalid document": func(t *testing.T, _ Layout, doc *sandboxconfig.RuntimeConfig) {
			doc.Proxy.ClientKey = newTestPKI(t).key // not the certificate's key
		},
		"unwritable target": func(t *testing.T, layout Layout, _ *sandboxconfig.RuntimeConfig) {
			// The state file's directory becomes a file: staging the last write
			// fails after every earlier one has been staged.
			if err := os.RemoveAll(filepath.Dir(layout.StatePath)); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Dir(layout.StatePath), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			layout := testLayout(t)
			writeManifest(t, layout, "30m0s")
			in, err := Open(layout, testOwner)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := in.Apply(testDocument(t, 1)); err != nil {
				t.Fatal(err)
			}

			next := testDocument(t, 2)
			next.Agent.IdleTimeout = "2h0m0s"
			next.SecretEnv = map[string]string{"NEXT": "discobox-sentinel-next"}
			next.Sources[0].Delivered = false
			breakIt(t, layout, &next)
			before := snapshot(t, layout)

			if _, err := in.Apply(next); err == nil {
				t.Fatal("Apply succeeded")
			}
			if in.Revision() != 1 {
				t.Fatalf("revision after a failed apply = %d, want 1", in.Revision())
			}
			if after := snapshot(t, layout); !equalSnapshots(before, after) {
				t.Fatalf("a failed apply changed files:\nbefore %v\nafter  %v", keys(before), keys(after))
			}
		})
	}
}

// A replacement that fails part-way puts back every target replaced before it.
func TestReplaceFailureRestoresEarlierTargets(t *testing.T) {
	layout := testLayout(t)
	writeManifest(t, layout, "30m0s")
	in, err := Open(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.Apply(testDocument(t, 1)); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, layout)

	next := testDocument(t, 2)
	next.SecretEnv = map[string]string{"NEXT": "discobox-sentinel-next"}
	next.Proxy.RegistryNamespace = ""
	ops, err := in.plan(next, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := stageAll(ops); err != nil {
		t.Fatal(err)
	}
	// Take the last op's staged file away, so every replacement before it
	// lands and the last one fails.
	last := &ops[len(ops)-1]
	if err := os.Remove(last.staged); err != nil {
		t.Fatal(err)
	}
	last.staged = filepath.Join(filepath.Dir(last.path), ".runtime-config-missing")
	err = replaceAll(ops)
	discardStaged(ops)
	if err == nil {
		t.Fatal("replaceAll succeeded")
	}
	if after := snapshot(t, layout); !equalSnapshots(before, after) {
		t.Fatalf("a failed replacement left changes:\nbefore %v\nafter  %v", keys(before), keys(after))
	}
}

func TestOpenReappliesTheKeptDocument(t *testing.T) {
	layout := testLayout(t)
	writeManifest(t, layout, "30m0s")
	first, err := Open(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	doc := testDocument(t, 3)
	if _, err := first.Apply(doc); err != nil {
		t.Fatal(err)
	}
	// A restart empties /run: the secrets file and the proxy material are gone.
	if err := os.RemoveAll(filepath.Dir(layout.SecretsPath)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(layout.ConfigDir, sandboxconfig.SourcesReadyFileName)); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(readFile(t, layout.StatePath), "PRIVATE KEY") {
		t.Fatal("the kept document carries the client key; client.key must be its only copy on disk")
	}

	second, err := Open(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision() != 3 {
		t.Fatalf("revision after restart = %d, want 3", second.Revision())
	}
	kept, ok := second.Applied()
	if !ok || !kept.SameDocument(doc) {
		t.Fatalf("kept document = %+v, %v", kept, ok)
	}
	if !strings.Contains(readFile(t, layout.SecretsPath), "discobox-sentinel-1") {
		t.Fatal("restart did not put the secrets file back")
	}
	if !exists(filepath.Join(layout.ConfigDir, sandboxconfig.SourcesReadyFileName)) {
		t.Fatal("restart did not put readiness back")
	}
	// The kept document goes on ordering deliveries.
	if held, err := second.Apply(testDocument(t, 2)); err != nil || held.Revision != 3 {
		t.Fatalf("older delivery after restart: held %d, err %v", held.Revision, err)
	}
}

// The kept document travels with an export; brought up under another pool, it
// is not this sandbox's, and its revision must not order what the new pool
// sends.
func TestOpenDoesNotRestoreAnotherOwnersDocument(t *testing.T) {
	layout := testLayout(t)
	first, err := Open(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	doc := testDocument(t, 9)
	doc.Proxy = nil
	if _, err := first.Apply(doc); err != nil {
		t.Fatal(err)
	}

	moved := testOwner
	moved.PoolID = "pool_2"
	second, err := Open(layout, moved)
	if err == nil {
		t.Fatal("restoring another pool's document reported no error")
	}
	if _, ok := second.Applied(); ok || second.Revision() != 0 {
		t.Fatalf("another pool's document was restored at revision %d", second.Revision())
	}
	fresh := testDocument(t, 1)
	fresh.Proxy = nil
	held, err := second.Apply(fresh)
	if err != nil || held.Revision != 1 {
		t.Fatalf("the new pool's first document: held %d, err %v", held.Revision, err)
	}
}

func TestOpenWithNothingKept(t *testing.T) {
	in, err := Open(testLayout(t), testOwner)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := in.Applied(); ok || in.Revision() != 0 {
		t.Fatal("a fresh intake reports a document")
	}
}

func TestReadinessFollowsDelivery(t *testing.T) {
	layout := testLayout(t)
	in, err := Open(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(layout.ConfigDir, sandboxconfig.SourcesReadyFileName)

	pending := testDocument(t, 1)
	pending.Sources = append(pending.Sources, sandboxconfig.RuntimeSource{Slug: "docs"})
	if _, err := in.Apply(pending); err != nil {
		t.Fatal(err)
	}
	if exists(ready) {
		t.Fatal("readiness published while a source is undelivered")
	}

	delivered := testDocument(t, 2)
	if _, err := in.Apply(delivered); err != nil {
		t.Fatal(err)
	}
	if !exists(ready) {
		t.Fatal("readiness not published once every source is delivered")
	}

	// A later document can withhold readiness again, as the pool clears its
	// marker when a delivery is pending.
	if _, err := in.Apply(withRevision(pending, 3)); err != nil {
		t.Fatal(err)
	}
	if exists(ready) {
		t.Fatal("readiness not withdrawn")
	}
}

func TestReadinessIsRemovedFirstAndPublishedLast(t *testing.T) {
	layout := testLayout(t)
	in := &Intake{layout: layout, owner: testOwner}
	ready := filepath.Join(layout.ConfigDir, sandboxconfig.SourcesReadyFileName)

	delivered, err := in.plan(testDocument(t, 1), true)
	if err != nil {
		t.Fatal(err)
	}
	// Readiness is last, after the state file: a failed state rename rolls
	// everything back, and must do so before any gate has opened.
	if got := delivered[len(delivered)-1]; got.path != ready || got.remove {
		t.Fatalf("last op = %+v, want the readiness write", got)
	}
	if got := delivered[len(delivered)-2]; got.path != layout.StatePath {
		t.Fatalf("second-to-last op = %+v, want the state file", got)
	}

	pending := testDocument(t, 1)
	pending.Sources[0].Delivered = false
	ops, err := in.plan(pending, true)
	if err != nil {
		t.Fatal(err)
	}
	if ops[0].path != ready || !ops[0].remove {
		t.Fatalf("first op = %+v, want the readiness removal", ops[0])
	}
}

func TestADocumentWithoutProxyRemovesTheMaterial(t *testing.T) {
	layout := testLayout(t)
	in, err := Open(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.Apply(testDocument(t, 1)); err != nil {
		t.Fatal(err)
	}
	bare := testDocument(t, 2)
	bare.Proxy = nil
	if _, err := in.Apply(bare); err != nil {
		t.Fatal(err)
	}
	for _, name := range proxyFileNames {
		if exists(filepath.Join(layout.ProxyDir, name)) {
			t.Fatalf("%s left behind by a document with no proxy", name)
		}
	}
}

func TestEmptyIdleTimeoutClearsTheManifestValue(t *testing.T) {
	layout := testLayout(t)
	writeManifest(t, layout, "30m0s")
	in, err := Open(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	doc := testDocument(t, 1)
	doc.Agent.IdleTimeout = ""
	if _, err := in.Apply(doc); err != nil {
		t.Fatal(err)
	}
	if effective, provenance := readManifestIdleTimeouts(t, layout); effective != "" || provenance != "" {
		t.Fatalf("manifest idle timeout = %q / %q, want both cleared", effective, provenance)
	}
}

func withRevision(doc sandboxconfig.RuntimeConfig, revision int64) sandboxconfig.RuntimeConfig {
	doc.Revision = revision
	return doc
}

func equalSnapshots(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for path, content := range a {
		if other, ok := b[path]; !ok || other != content {
			return false
		}
	}
	return true
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
