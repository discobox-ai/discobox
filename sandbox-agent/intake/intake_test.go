package intake

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"maps"
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

// testPool is where the bootstrap says the pool serves the sandbox.
//
//nolint:gosec // G101: service URLs, not credentials.
var testPool = &sandboxconfig.PoolEndpoints{
	Proxy:       "https://pool:17443",
	Credentials: "https://pool:17444",
	DNS:         "pool:853",
	BuildKit:    "https://pool:17445",
	ServerName:  "pool",
}

func testConfig(layout Layout, owner Owner) Config {
	return Config{Layout: layout, Owner: owner, Pool: testPool}
}

func openIntake(layout Layout, owner Owner) (*Intake, error) {
	return Open(context.Background(), testConfig(layout, owner))
}

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
			MTLSCA:            pki.ca,
			MITMCA:            pki.ca,
			ClientCert:        pki.cert,
			ClientKey:         pki.key,
			RegistryNamespace: "ns-0123abcd",
		},
		Sources: []sandboxconfig.RuntimeSource{{
			Slug:      "primary",
			Target:    "/workspace/primary",
			OriginURL: "https://pool/origins/primary",
			Commit:    "0123456789abcdef0123456789abcdef01234567",
			Delivered: true,
		}},
	}
}

// writeManifest places a create-time sandbox.json, which the intake must never
// touch: it is the static bootstrap (ADR 26-10-08-127 §1).
func writeManifest(t *testing.T, layout Layout) string {
	t.Helper()
	data := `{"apiVersion":"` + sandboxconfig.APIVersion + `","sandboxId":"sbx_1","agentRuntime":{"listenAddress":":3003"}}`
	if err := os.MkdirAll(layout.ConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.ConfigDir, "sandbox.json"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return data
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
	manifest := writeManifest(t, layout)
	in, err := openIntake(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	doc := testDocument(t, 1)
	applied, err := in.Apply(context.Background(), doc)
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

	if got := readFile(t, filepath.Join(layout.ConfigDir, "sandbox.json")); got != manifest {
		t.Fatalf("sandbox.json was rewritten: %s", got)
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
	// The pool's half from the bootstrap, the sandbox's own listeners, and the
	// delivered credential where this sandbox keeps it (ADR 26-10-08-127 §4).
	if egress.UpstreamURL != testPool.Proxy || egress.CredentialsURL != testPool.Credentials || egress.DNSServer != testPool.DNS ||
		egress.ServerName != testPool.ServerName ||
		egress.ListenAddress != sandboxconfig.SandboxEgressListenAddress ||
		egress.DNSListenAddress != sandboxconfig.SandboxDNSListenAddress ||
		egress.ClientKeyPath != filepath.Join(layout.ProxyDir, clientKeyFile) ||
		egress.MTLSCAPath != filepath.Join(layout.ProxyDir, mtlsCAFile) {
		t.Fatalf("egress bridge = %+v", egress)
	}
	var buildkit, docker bridgeFile
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(layout.ProxyDir, buildKitBridgeFile))), &buildkit); err != nil {
		t.Fatal(err)
	}
	if buildkit.UpstreamURL != testPool.BuildKit || buildkit.ListenAddress != sandboxconfig.SandboxBuildKitListenAddress {
		t.Fatalf("buildkit bridge = %+v", buildkit)
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(layout.ProxyDir, nestedDockerBridgeFile))), &docker); err != nil {
		t.Fatal(err)
	}
	if docker.UpstreamURL != testPool.Proxy || docker.ListenAddress != "" {
		t.Fatalf("nested-docker bridge = %+v, want the pool proxy and no listener", docker)
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
	in, err := openIntake(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	newer := testDocument(t, 5)
	if _, err := in.Apply(context.Background(), newer); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, layout)

	older := testDocument(t, 4)
	older.SecretEnv = map[string]string{"OLD": "discobox-sentinel-old"}
	held, err := in.Apply(context.Background(), older)
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
	in, err := openIntake(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	doc := testDocument(t, 2)
	if _, err := in.Apply(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Apply(context.Background(), doc); err != nil {
		t.Fatalf("re-delivering the applied document: %v", err)
	}
	different := doc
	different.SecretEnv = map[string]string{"OTHER": "discobox-sentinel-2"}
	if _, err := in.Apply(context.Background(), different); !errors.Is(err, ErrConflict) {
		t.Fatalf("a different document under the applied revision: err = %v, want ErrConflict", err)
	}
}

// Only a newer document is validated: an older one is ignored and anything but
// the held document under the held revision is a conflict, whatever its shape.
func TestOrderingComesBeforeValidation(t *testing.T) {
	layout := testLayout(t)
	in, err := openIntake(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.Apply(context.Background(), testDocument(t, 5)); err != nil {
		t.Fatal(err)
	}
	malformed := func(revision int64) sandboxconfig.RuntimeConfig {
		doc := testDocument(t, revision)
		doc.Agent.IdleTimeout = "soon"
		return doc
	}
	if held, err := in.Apply(context.Background(), malformed(4)); err != nil || held.Revision != 5 {
		t.Fatalf("a malformed older document: held %d, err %v; want it ignored", held.Revision, err)
	}
	if _, err := in.Apply(context.Background(), malformed(5)); !errors.Is(err, ErrConflict) {
		t.Fatalf("a malformed document under the held revision: err = %v, want ErrConflict", err)
	}
	if _, err := in.Apply(context.Background(), malformed(6)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a malformed newer document: err = %v, want ErrInvalid", err)
	}
	if in.Revision() != 5 {
		t.Fatalf("revision = %d, want 5", in.Revision())
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
			writeManifest(t, layout)
			in, err := openIntake(layout, testOwner)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := in.Apply(context.Background(), testDocument(t, 1)); err != nil {
				t.Fatal(err)
			}

			next := testDocument(t, 2)
			next.Agent.IdleTimeout = "2h0m0s"
			next.SecretEnv = map[string]string{"NEXT": "discobox-sentinel-next"}
			next.Sources[0].Delivered = false
			breakIt(t, layout, &next)
			before := snapshot(t, layout)

			if _, err := in.Apply(context.Background(), next); err == nil {
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
	writeManifest(t, layout)
	in, err := openIntake(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.Apply(context.Background(), testDocument(t, 1)); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, layout)

	next := testDocument(t, 2)
	next.SecretEnv = map[string]string{"NEXT": "discobox-sentinel-next"}
	next.Proxy.RegistryNamespace = ""
	body, tail, err := in.plan(next, true)
	if err != nil {
		t.Fatal(err)
	}
	ops := append(body, tail...)
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
	writeManifest(t, layout)
	first, err := openIntake(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	doc := testDocument(t, 3)
	if _, err := first.Apply(context.Background(), doc); err != nil {
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

	second, err := openIntake(layout, testOwner)
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
	if held, err := second.Apply(context.Background(), testDocument(t, 2)); err != nil || held.Revision != 3 {
		t.Fatalf("older delivery after restart: held %d, err %v", held.Revision, err)
	}
}

// The kept document travels with an export; brought up under another pool, it
// is not this sandbox's, and its revision must not order what the new pool
// sends.
func TestOpenDoesNotRestoreAnotherOwnersDocument(t *testing.T) {
	layout := testLayout(t)
	first, err := openIntake(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	doc := testDocument(t, 9)
	doc.Proxy = nil
	if _, err := first.Apply(context.Background(), doc); err != nil {
		t.Fatal(err)
	}

	moved := testOwner
	moved.PoolID = "pool_2"
	second, err := openIntake(layout, moved)
	if err == nil {
		t.Fatal("restoring another pool's document reported no error")
	}
	if _, ok := second.Applied(); ok || second.Revision() != 0 {
		t.Fatalf("another pool's document was restored at revision %d", second.Revision())
	}
	fresh := testDocument(t, 1)
	fresh.Proxy = nil
	held, err := second.Apply(context.Background(), fresh)
	if err != nil || held.Revision != 1 {
		t.Fatalf("the new pool's first document: held %d, err %v", held.Revision, err)
	}
}

func TestOpenWithNothingKept(t *testing.T) {
	in, err := openIntake(testLayout(t), testOwner)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := in.Applied(); ok || in.Revision() != 0 {
		t.Fatal("a fresh intake reports a document")
	}
}

func TestReadinessFollowsDelivery(t *testing.T) {
	layout := testLayout(t)
	in, err := openIntake(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(layout.ConfigDir, sandboxconfig.SourcesReadyFileName)

	pending := testDocument(t, 1)
	pending.Sources = append(pending.Sources, sandboxconfig.RuntimeSource{Slug: "docs"})
	if _, err := in.Apply(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	if exists(ready) {
		t.Fatal("readiness published while a source is undelivered")
	}

	delivered := testDocument(t, 2)
	if _, err := in.Apply(context.Background(), delivered); err != nil {
		t.Fatal(err)
	}
	if !exists(ready) {
		t.Fatal("readiness not published once every source is delivered")
	}

	// A later document can withhold readiness again, as the pool clears its
	// marker when a delivery is pending.
	if _, err := in.Apply(context.Background(), withRevision(pending, 3)); err != nil {
		t.Fatal(err)
	}
	if exists(ready) {
		t.Fatal("readiness not withdrawn")
	}
}

func TestReadinessIsRemovedFirstAndPublishedLast(t *testing.T) {
	layout := testLayout(t)
	in := &Intake{layout: layout, owner: testOwner, pool: testPool}
	ready := filepath.Join(layout.ConfigDir, sandboxconfig.SourcesReadyFileName)

	body, tail, err := in.plan(testDocument(t, 1), true)
	if err != nil {
		t.Fatal(err)
	}
	// Removed first even when this document grants readiness: a marker left
	// up from the previous one would be open over this one's files.
	if body[0].path != ready || !body[0].remove {
		t.Fatalf("first op = %+v, want the readiness removal", body[0])
	}
	// The tail says the delivery took — the state file, then readiness — and
	// goes in only after the body's readers are started: a failed state rename
	// rolls everything back before any gate has opened.
	if len(tail) != 2 || tail[0].path != layout.StatePath || tail[1].path != ready || tail[1].remove {
		t.Fatalf("tail = %+v, want the state file then the readiness write", tail)
	}
	for _, o := range body {
		if o.path == layout.StatePath {
			t.Fatal("the state file is written before the readers are started")
		}
	}

	pending := testDocument(t, 1)
	pending.Sources[0].Delivered = false
	body, tail, err = in.plan(pending, true)
	if err != nil {
		t.Fatal(err)
	}
	if body[0].path != ready || !body[0].remove || len(tail) != 1 || tail[0].path != layout.StatePath {
		t.Fatalf("first op = %+v, tail %+v; want the readiness removal and only the state file after", body[0], tail)
	}
}

// What reads the proxy material is started after the files are in place and
// before readiness is published, so a harness never launches over a hop that
// is not up (ADR 26-10-08-127 §5). A delivery that changes none of the
// material starts nothing.
func TestApplyActivatesWhatChangedBeforeReadiness(t *testing.T) {
	layout := testLayout(t)
	ready := filepath.Join(layout.ConfigDir, sandboxconfig.SourcesReadyFileName)
	var calls [][]string
	cfg := testConfig(layout, testOwner)
	cfg.Activate = func(_ context.Context, changed []string) error {
		if exists(ready) {
			t.Error("activated after readiness was published")
		}
		if !exists(filepath.Join(layout.ProxyDir, egressBridgeFile)) {
			t.Error("activated before the bridge config was in place")
		}
		calls = append(calls, changed)
		return nil
	}
	in, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	doc := testDocument(t, 1)
	if _, err := in.Apply(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !containsPath(calls[0], filepath.Join(layout.ProxyDir, egressBridgeFile)) || !containsPath(calls[0], filepath.Join(layout.ProxyDir, mitmCAFile)) {
		t.Fatalf("activations = %v, want one naming the new material", calls)
	}
	if !exists(ready) {
		t.Fatal("readiness not published")
	}

	// Only the secrets change: the units reading the proxy material are left
	// alone.
	calls = nil
	next := withRevision(doc, 2)
	next.SecretEnv = map[string]string{"NEXT": "discobox-sentinel-next"}
	if _, err := in.Apply(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if actions := unitActions(layout.ProxyDir, call); len(actions) != 0 {
			t.Fatalf("a secrets-only delivery touched units %+v", actions)
		}
	}
}

// A reader that will not start keeps the delivery from publishing readiness or
// counting as applied, so the pool delivers again — and the next delivery,
// though it changes no file, tries the same readers again.
func TestAFailedActivationWithholdsReadinessUntilARetrySucceeds(t *testing.T) {
	layout := testLayout(t)
	ready := filepath.Join(layout.ConfigDir, sandboxconfig.SourcesReadyFileName)
	fail := true
	var calls [][]string
	cfg := testConfig(layout, testOwner)
	cfg.Activate = func(_ context.Context, changed []string) error {
		calls = append(calls, changed)
		if fail {
			return errors.New("discobox-trust-ca.service failed")
		}
		return nil
	}
	in, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	doc := testDocument(t, 1)
	if _, err := in.Apply(context.Background(), doc); !errors.Is(err, ErrActivation) {
		t.Fatalf("Apply with a failing reader: %v, want ErrActivation", err)
	}
	if exists(ready) || in.Revision() != 0 {
		t.Fatalf("a failed activation published readiness (%v) or applied revision %d", exists(ready), in.Revision())
	}
	fail = false
	if _, err := in.Apply(context.Background(), doc); err != nil {
		t.Fatalf("redelivering the same document: %v", err)
	}
	if !exists(ready) || in.Revision() != 1 {
		t.Fatalf("after a successful retry: ready %v, revision %d", exists(ready), in.Revision())
	}
	if len(calls) != 2 || !containsPath(calls[1], filepath.Join(layout.ProxyDir, mitmCAFile)) {
		t.Fatalf("activations = %v, want the retry to try the same readers though no file changed", calls)
	}
}

// A delivery whose readers did not come up is not kept: an agent that restarts
// before the pool redelivers neither reports that revision nor is ready, so the
// pool delivers it again.
func TestAFailedActivationIsNotKept(t *testing.T) {
	layout := testLayout(t)
	fail := false
	cfg := testConfig(layout, testOwner)
	cfg.Activate = func(context.Context, []string) error {
		if fail {
			return errors.New("discobox-proxy-bridge.service did not start listening")
		}
		return nil
	}
	in, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.Apply(context.Background(), testDocument(t, 1)); err != nil {
		t.Fatal(err)
	}
	fail = true
	if _, err := in.Apply(context.Background(), testDocument(t, 2)); !errors.Is(err, ErrActivation) {
		t.Fatalf("Apply with a failing reader: %v, want ErrActivation", err)
	}

	// The body of revision 2 is in place, its keypair included, and the state
	// file still names revision 1, whose certificate does not match that key:
	// the restore refuses it. Either way the restarted agent does not claim
	// revision 2 and publishes no readiness, so the pool delivers again.
	fail = false
	restarted, _ := Open(context.Background(), cfg)
	if restarted.Revision() == 2 {
		t.Fatal("after a restart the sandbox reports the revision whose readers never started")
	}
	if exists(filepath.Join(layout.ConfigDir, sandboxconfig.SourcesReadyFileName)) {
		t.Fatal("after a restart the sandbox is ready over a delivery whose readers never started")
	}
}

func TestUnitActionsFollowTheFilesEachUnitReads(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{mitmCAFile, egressBridgeFile, mtlsCAFile, clientCertFile, clientKeyFile} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	verbs := func(changed ...string) map[string]unitVerb {
		paths := make([]string, 0, len(changed))
		for _, name := range changed {
			paths = append(paths, filepath.Join(dir, name))
		}
		out := map[string]unitVerb{}
		for _, action := range unitActions(dir, paths) {
			out[action.unit.name] = action.verb
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		changed []string
		want    map[string]unitVerb
	}{
		{
			// The trust store reads only the MITM CA, once, when it runs.
			name:    "MITM CA",
			changed: []string{mitmCAFile},
			want:    map[string]unitVerb{"discobox-trust-ca.service": verbRestart},
		},
		{
			// A renewal: a running bridge takes the keypair itself, so a
			// present config is only started — a no-op when it is up — and an
			// absent one stops (#62).
			name:    "renewed keypair",
			changed: []string{clientCertFile, clientKeyFile},
			want: map[string]unitVerb{
				"discobox-proxy-bridge.service":        verbStart,
				"discobox-buildkit-bridge.service":     verbStop,
				"discobox-proxy-bridge-docker.service": verbStop,
			},
		},
		{
			name:    "new mTLS CA",
			changed: []string{mtlsCAFile},
			want: map[string]unitVerb{
				"discobox-proxy-bridge.service":        verbStart,
				"discobox-buildkit-bridge.service":     verbStop,
				"discobox-proxy-bridge-docker.service": verbStop,
			},
		},
		{
			// A bridge reads its config only when it starts.
			name:    "bridge config and keypair",
			changed: []string{egressBridgeFile, clientKeyFile},
			want: map[string]unitVerb{
				"discobox-proxy-bridge.service":        verbRestart,
				"discobox-buildkit-bridge.service":     verbStop,
				"discobox-proxy-bridge-docker.service": verbStop,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := verbs(tc.changed...); !maps.Equal(got, tc.want) {
				t.Fatalf("changed %v: %v, want %v", tc.changed, got, tc.want)
			}
		})
	}
	if got := unitActions(dir, []string{filepath.Join(t.TempDir(), mitmCAFile)}); len(got) != 0 {
		t.Fatalf("a file outside the proxy directory touched units %+v", got)
	}
}

// A sandbox whose bootstrap names no pool has no bridges to render, whatever
// material it is delivered.
func TestNoBridgesWithoutAPool(t *testing.T) {
	layout := testLayout(t)
	cfg := testConfig(layout, testOwner)
	cfg.Pool = nil
	in, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.Apply(context.Background(), testDocument(t, 1)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{egressBridgeFile, nestedDockerBridgeFile, buildKitBridgeFile} {
		if exists(filepath.Join(layout.ProxyDir, name)) {
			t.Fatalf("%s rendered with no pool", name)
		}
	}
	if !exists(filepath.Join(layout.ProxyDir, clientKeyFile)) {
		t.Fatal("the delivered keypair was not written")
	}
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

func TestADocumentWithoutProxyRemovesTheMaterial(t *testing.T) {
	layout := testLayout(t)
	in, err := openIntake(layout, testOwner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := in.Apply(context.Background(), testDocument(t, 1)); err != nil {
		t.Fatal(err)
	}
	bare := testDocument(t, 2)
	bare.Proxy = nil
	if _, err := in.Apply(context.Background(), bare); err != nil {
		t.Fatal(err)
	}
	for _, name := range proxyFileNames {
		if exists(filepath.Join(layout.ProxyDir, name)) {
			t.Fatalf("%s left behind by a document with no proxy", name)
		}
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
