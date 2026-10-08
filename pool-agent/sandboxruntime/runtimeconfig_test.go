package sandboxruntime

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"

	"github.com/discobox-ai/discobox/layout"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
	"github.com/discobox-ai/discobox/sandboxconfig"
)

func runtimeConfigTestRuntime(t *testing.T) *DockerSandboxRuntime {
	t.Helper()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &DockerSandboxRuntime{
		root:               layout.ContainerAt(t.TempDir()),
		projectID:          "proj_a",
		poolID:             "pool_a",
		identityKey:        key,
		sandboxIdleTimeout: 20 * time.Minute,
	}
	// The sandbox's tree, which deciding a document requires.
	if err := os.MkdirAll(r.sandboxRoot("sbx_1"), 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

// Deciding issues material and writes the record, so it is refused for a
// sandbox this pool no longer holds, or holds archived: a convergence or a
// secret update arriving late must not put back what a delete took away.
func TestDecideRuntimeConfigRefusesASandboxThatIsGone(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	if _, err := r.decideRuntimeConfig("sbx_gone", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deciding for a sandbox with no tree: %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(r.sandboxRoot("sbx_gone")); !os.IsNotExist(err) {
		t.Fatalf("deciding recreated the deleted sandbox's tree: %v", err)
	}
	if err := writeSandboxArchiveMarker(r.sandboxRoot("sbx_1"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.decideRuntimeConfig("sbx_1", nil); !errors.Is(err, ErrArchived) {
		t.Fatalf("deciding for an archived sandbox: %v, want ErrArchived", err)
	}
}

// The bootstrap is static and names what the sandbox trusts and where its pool
// is, and nothing that can change while it exists: no idle timeout, no secret,
// no private key (ADR 26-10-08-127 §1).
func TestSandboxBootstrapNamesThePoolAndHoldsNoSecret(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	req := &workerapimodel.PoolSandboxCreateRequest{SandboxId: "sandbox-1"}
	doc := buildSandboxDocument(linuxPaths, "proj_a", "sandbox-1", "pool_a", "cp-key", r.poolPublicKey(), "sha256:image", req, nil, nil)
	cfg, _ := sandboxconfig.Effective(doc)
	if cfg.Provider.PublicKeys[sandboxconfig.ControlPlanePublicKeyName] != "cp-key" || cfg.Provider.PublicKeys[sandboxconfig.PoolPublicKeyName] != r.poolPublicKey() {
		t.Fatalf("public keys = %v", cfg.Provider.PublicKeys)
	}
	if !cfg.Provider.AwaitsRuntimeConfig() {
		t.Fatal("a bootstrap naming the pool's key does not await a runtime config")
	}
	if cfg.Provider.Pool == nil || cfg.Provider.Pool.Proxy == "" || cfg.Provider.Pool.BuildKit == "" {
		t.Fatalf("pool endpoints = %+v", cfg.Provider.Pool)
	}
	data, err := marshalSandboxDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"PRIVATE KEY", "idleTimeout"} {
		if strings.Contains(string(data), leaked) {
			t.Fatalf("the bootstrap carries %q: %s", leaked, data)
		}
	}
}

// Deciding a document again is free: only a change makes a new revision, and
// the pool's own record never holds the sandbox's private key.
func TestDecideRuntimeConfigRevisesOnlyOnChange(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	setSecret := func(value string) func(*sandboxconfig.RuntimeConfig) {
		return func(doc *sandboxconfig.RuntimeConfig) { doc.SecretEnv = map[string]string{"GH_TOKEN": value} }
	}
	first, err := r.decideRuntimeConfig("sbx_1", setSecret("discobox-sentinel-1"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 || first.Agent.IdleTimeout != "20m0s" || first.Proxy == nil || first.Proxy.ClientKey == "" {
		t.Fatalf("first decision = rev %d, agent %+v, proxy %v", first.Revision, first.Agent, first.Proxy != nil)
	}
	if err := first.Validate(linuxPaths); err != nil {
		t.Fatalf("the decided document is not one the sandbox would apply: %v", err)
	}
	again, err := r.decideRuntimeConfig("sbx_1", nil)
	if err != nil || again.Revision != 1 || again.SecretEnv["GH_TOKEN"] != "discobox-sentinel-1" {
		t.Fatalf("deciding again = rev %d %v, err %v; want revision 1 and the recorded secrets", again.Revision, again.SecretEnv, err)
	}
	rotated, err := r.decideRuntimeConfig("sbx_1", setSecret("discobox-sentinel-2"))
	if err != nil || rotated.Revision != 2 {
		t.Fatalf("a rotated secret = rev %d, err %v; want 2", rotated.Revision, err)
	}
	r.sandboxIdleTimeout = time.Hour
	retimed, err := r.decideRuntimeConfig("sbx_1", nil)
	if err != nil || retimed.Revision != 3 || retimed.Agent.IdleTimeout != "1h0m0s" {
		t.Fatalf("a changed idle timeout = rev %d %+v, err %v; want 3", retimed.Revision, retimed.Agent, err)
	}
	record, err := os.ReadFile(r.runtimeConfigRecordPath("sbx_1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(record), "PRIVATE KEY") {
		t.Fatal("the pool's record carries the sandbox's private key")
	}
}

// A running sandbox's certificate is renewed before it expires (ADR 0126 §7):
// every status poll decides the document again, and a certificate inside its
// renewal window is reissued there, which makes a new revision the poll then
// delivers. Once renewed, deciding again is free.
func TestDecideRuntimeConfigRenewsACertificateDueForRenewal(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	first, err := r.decideRuntimeConfig("sbx_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := r.decideRuntimeConfig("sbx_1", nil); err != nil || again.Proxy.ClientCert != first.Proxy.ClientCert {
		t.Fatalf("a certificate far from expiry was reissued (err %v)", err)
	}

	// Stand the sandbox's certificate ten days from expiry, inside the window.
	bundle, err := proxyagent.PrepareBundle(r.root, r.projectID, r.poolID)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(bundle.MTLSCA.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "sbx_1"},
		NotBefore:    time.Now().Add(-355 * 24 * time.Hour),
		NotAfter:     time.Now().Add(10 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, ca, &key.PublicKey, bundle.MTLSCA.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	clientDir := filepath.Join(bundle.Dir, "clients", "sbx_1")
	if err := os.WriteFile(filepath.Join(clientDir, "client.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clientDir, "client.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}

	renewed, err := r.decideRuntimeConfig("sbx_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Revision != first.Revision+1 {
		t.Fatalf("renewal decided revision %d, want %d", renewed.Revision, first.Revision+1)
	}
	block, _ := pem.Decode([]byte(renewed.Proxy.ClientCert))
	if block == nil {
		t.Fatal("the renewed document carries no certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !leaf.NotAfter.After(time.Now().Add(300 * 24 * time.Hour)) {
		t.Fatalf("renewed certificate runs to %s, want about a year out", leaf.NotAfter)
	}
	if _, err := tls.X509KeyPair([]byte(renewed.Proxy.ClientCert), []byte(renewed.Proxy.ClientKey)); err != nil {
		t.Fatalf("the renewed document's keypair does not match: %v", err)
	}
	if err := renewed.Validate(linuxPaths); err != nil {
		t.Fatalf("the renewed document is not one the sandbox would apply: %v", err)
	}
	if again, err := r.decideRuntimeConfig("sbx_1", nil); err != nil || again.Revision != renewed.Revision {
		t.Fatalf("deciding after the renewal = rev %d, err %v; want %d", again.Revision, err, renewed.Revision)
	}
}

// fakeIntake is a sandbox agent's runtime-config route: it verifies the
// pool-signed token the way the sandbox agent does and holds the newest
// revision it was sent, answering an older one with what it holds.
type fakeIntake struct {
	t      *testing.T
	key    ed25519.PublicKey
	status int

	mu   sync.Mutex
	held sandboxconfig.RuntimeConfig
	puts int
}

func (f *fakeIntake) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
		return // the sandbox agent's port probe, inside a discobox; see REVIEW.md
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if f.status != 0 {
		http.Error(w, "no", f.status)
		return
	}
	if req.Method != http.MethodPut || req.URL.Path != "/api/projects/proj_a/sandboxes/sbx_1/runtime-config" {
		f.t.Errorf("request %s %s", req.Method, req.URL.Path)
	}
	public, err := paseto.NewV4AsymmetricPublicKeyFromEd25519(f.key)
	if err != nil {
		f.t.Fatal(err)
	}
	parser := paseto.NewParserForValidNow()
	parser.AddRule(paseto.ForAudience("sandbox-agent"))
	token, err := parser.ParseV4Public(public, strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	var scopes []string
	_ = token.Get("scopes", &scopes)
	poolID, _ := token.GetString("pool_id")
	if len(scopes) != 1 || scopes[0] != sandboxconfig.RuntimeConfigScope || poolID != "pool_a" {
		http.Error(w, "scope", http.StatusForbidden)
		return
	}
	var doc sandboxconfig.RuntimeConfig
	body, _ := io.ReadAll(req.Body)
	if err := json.Unmarshal(body, &doc); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if doc.Proxy == nil || doc.Proxy.ClientKey == "" {
		f.t.Error("a delivery without the sandbox's client key")
	}
	if doc.Revision > f.held.Revision {
		f.held = doc
	}
	if err := json.NewEncoder(w).Encode(f.held); err != nil {
		f.t.Error(err)
	}
}

func intakeDialer(t *testing.T, handler http.Handler) Dialer {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
}

func TestPutRuntimeConfigDeliversAPoolSignedDocument(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	public, _ := r.identityKey.Public().(ed25519.PublicKey)
	if r.poolPublicKey() != base64.StdEncoding.EncodeToString(public) {
		t.Fatal("the bootstrap's pool key is not the key deliveries are signed with")
	}
	intake := &fakeIntake{t: t, key: public}
	doc, err := r.decideRuntimeConfig("sbx_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.putRuntimeConfig(context.Background(), intakeDialer(t, intake), "sbx_1", doc); err != nil {
		t.Fatal(err)
	}
	if intake.held.Revision != doc.Revision {
		t.Fatalf("the sandbox holds revision %d, want %d", intake.held.Revision, doc.Revision)
	}
}

// A sandbox that kept a newer document than the pool's record — the record lost
// with the pool's state, or the sandbox moved here — answers with it; the pool
// moves its record past it and delivers again, so its next document is applied.
func TestPutRuntimeConfigAdvancesPastWhatTheSandboxHolds(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	public, _ := r.identityKey.Public().(ed25519.PublicKey)
	intake := &fakeIntake{t: t, key: public, held: sandboxconfig.RuntimeConfig{Revision: 7}}
	doc, err := r.decideRuntimeConfig("sbx_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.putRuntimeConfig(context.Background(), intakeDialer(t, intake), "sbx_1", doc); err != nil {
		t.Fatal(err)
	}
	if intake.held.Revision != 8 || intake.puts != 2 {
		t.Fatalf("the sandbox holds revision %d after %d deliveries, want 8 after 2", intake.held.Revision, intake.puts)
	}
	recorded, _, err := r.readRuntimeConfig("sbx_1")
	if err != nil || recorded.Revision != 8 {
		t.Fatalf("the pool's record = revision %d, err %v; want 8", recorded.Revision, err)
	}
}

// An agent with no intake is refused rather than staged around
// (ADR 26-10-08-127 §7); any other failure is the status poll's to retry.
func TestPutRuntimeConfigToAnAgentWithoutTheIntake(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	doc, err := r.decideRuntimeConfig("sbx_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = r.putRuntimeConfig(context.Background(), intakeDialer(t, &fakeIntake{t: t, status: http.StatusNotFound}), "sbx_1", doc)
	if !errors.Is(err, ErrRuntimeConfigUnsupported) {
		t.Fatalf("delivering to an agent without the intake: %v", err)
	}
	if logRuntimeConfigFailure(context.Background(), "sbx_1", err) == nil {
		t.Fatal("an agent without the intake was left to the status poll")
	}
	// A refusal no retry can change fails the boot too.
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity} {
		refused := r.putRuntimeConfig(context.Background(), intakeDialer(t, &fakeIntake{t: t, status: status}), "sbx_1", doc)
		if !errors.Is(refused, ErrRuntimeConfigRefused) || logRuntimeConfigFailure(context.Background(), "sbx_1", refused) == nil {
			t.Fatalf("status %d: %v, want a refusal the caller sees", status, refused)
		}
	}
	failed := r.putRuntimeConfig(context.Background(), intakeDialer(t, &fakeIntake{t: t, status: http.StatusServiceUnavailable}), "sbx_1", doc)
	if failed == nil || logRuntimeConfigFailure(context.Background(), "sbx_1", failed) != nil {
		t.Fatalf("a transient failure: %v, want it logged for the status poll", failed)
	}
}

// A delivery that landed is not logged as one that did not.
func TestLogRuntimeConfigFailureIsQuietOnSuccess(t *testing.T) {
	var logged strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	if err := logRuntimeConfigFailure(context.Background(), "sbx_1", nil); err != nil || logged.Len() != 0 {
		t.Fatalf("a nil delivery error = %v, logged %q", err, logged.String())
	}
}
