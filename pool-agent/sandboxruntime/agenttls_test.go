package sandboxruntime

import (
	"archive/tar"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
)

// The document renews the agent's keypair for a sandbox whose bootstrap told
// it to serve TLS, and for no other: an agent told plain HTTP may predate the
// field, and refuse a whole document over it. The record never holds the key
// (ADR 26-10-29-481 §3).
func TestTheDocumentRenewsTheAgentsKeypairWhereItServesTLS(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	if err := r.recordAgentTLS("sbx_1", false); err != nil {
		t.Fatal(err)
	}
	plain, err := r.decideRuntimeConfig("sbx_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain.Agent.TLS != nil {
		t.Fatal("the document renews the keypair of an agent told to serve plain HTTP")
	}

	if err := r.recordAgentTLS("sbx_1", true); err != nil {
		t.Fatal(err)
	}
	served, err := r.decideRuntimeConfig("sbx_1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if served.Agent.TLS == nil || served.Revision != plain.Revision+1 {
		t.Fatalf("for an agent told to serve TLS: tls %v, revision %d after %d; want the keypair in a new revision", served.Agent.TLS != nil, served.Revision, plain.Revision)
	}
	if _, err := tls.X509KeyPair([]byte(served.Agent.TLS.Cert), []byte(served.Agent.TLS.Key)); err != nil {
		t.Fatalf("the renewed keypair does not load: %v", err)
	}
	record, err := os.ReadFile(r.runtimeConfigRecordPath("sbx_1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(record), "PRIVATE KEY") {
		t.Fatal("the pool's record of the document holds a private key")
	}
}

// A sandbox with no record was made before the setting, and is reached over
// plain HTTP as it was made.
func TestASandboxMadeBeforeTheSettingIsReachedOverPlainHTTP(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	if serves, err := r.agentServesTLS("sbx_1"); err != nil || serves {
		t.Fatalf("agentServesTLS = %v, %v; want false for a sandbox with no record", serves, err)
	}
}

// agentServer serves HTTPS with the keypair the pool issued sandboxID's agent.
func agentServer(t *testing.T, r *DockerSandboxRuntime, sandboxID string) *httptest.Server {
	t.Helper()
	material, err := proxyagent.EnsureSandboxMaterial(r.root, r.projectID, r.poolID, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair([]byte(material.Agent.Cert), []byte(material.Agent.Key))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "agent")
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func plainDial(address string) Dialer {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", address)
	}
}

func getThrough(t *testing.T, dial Dialer) (string, error) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) }}}
	resp, err := client.Get("http://agent/healthz")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

// A sandbox told to serve TLS is reached over TLS alone, verified against the
// pool's CA for the sandbox's private name, wherever the pool dialed it:
// another sandbox's agent answering at that address is refused.
func TestTheAgentIsDialedOverTLSWhereItsBootstrapSaysSo(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	if err := os.MkdirAll(r.sandboxRoot("sbx_2"), 0o755); err != nil {
		t.Fatal(err)
	}
	own := agentServer(t, r, "sbx_1")
	address := own.Listener.Addr().String()

	if err := r.recordAgentTLS("sbx_1", false); err != nil {
		t.Fatal(err)
	}
	dial, err := r.agentDialer("sbx_1", plainDial(address))
	if err != nil {
		t.Fatal(err)
	}
	// A TLS-only server answers plain HTTP with a refusal of its own.
	if body, _ := getThrough(t, dial); body == "agent" {
		t.Fatal("a sandbox told plain HTTP was spoken TLS to")
	}

	if err := r.recordAgentTLS("sbx_1", true); err != nil {
		t.Fatal(err)
	}
	dial, err = r.agentDialer("sbx_1", plainDial(address))
	if err != nil {
		t.Fatal(err)
	}
	if body, err := getThrough(t, dial); err != nil || body != "agent" {
		t.Fatalf("over TLS to the sandbox's own agent: %q, %v", body, err)
	}

	// sbx_2's agent answering where the pool means to reach sbx_1.
	peer := agentServer(t, r, "sbx_2")
	dial, err = r.agentDialer("sbx_1", plainDial(peer.Listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	var verify *tls.CertificateVerificationError
	if _, err := getThrough(t, dial); !errors.As(err, &verify) {
		t.Fatalf("over TLS to a peer's agent: %v, want its certificate refused", err)
	}
}

func readTarTree(t *testing.T, tree io.Reader) (map[string]*tar.Header, map[string]string) {
	t.Helper()
	entries := map[string]*tar.Header{}
	bodies := map[string]string{}
	tr := tar.NewReader(tree)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return entries, bodies
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name], bodies[header.Name] = header, string(body)
	}
}

// A machine's place step copies the agent's keypair beside sandbox.json where
// its agent serves TLS, its key root's alone, and nothing where it does not.
func TestBootstrapTreeCarriesTheAgentsKeypairWhereItServesTLS(t *testing.T) {
	dir := "etc/discobox/" + sandboxconfig.AgentTLSDirName + "/"
	entries, bodies := readTarTree(t, bootstrapTree([]byte(`{}`), &sandboxconfig.AgentTLS{Cert: "cert", Key: "key"}))
	cert, key := entries[dir+sandboxconfig.AgentTLSCertName], entries[dir+sandboxconfig.AgentTLSKeyName]
	if cert == nil || key == nil || entries["etc/discobox/sandbox.json"] == nil {
		t.Fatalf("tree holds %v, want sandbox.json and the agent's keypair", entries)
	}
	if bodies[dir+sandboxconfig.AgentTLSKeyName] != "key" || key.Mode != 0o600 || cert.Mode != 0o644 {
		t.Fatalf("key %q mode %o, cert mode %o; want the key root's alone", bodies[dir+sandboxconfig.AgentTLSKeyName], key.Mode, cert.Mode)
	}
	plain, _ := readTarTree(t, bootstrapTree([]byte(`{}`), nil))
	if plain[dir] != nil || plain["etc/discobox/sandbox.json"] == nil {
		t.Fatalf("tree of an agent told plain HTTP holds %v, want sandbox.json alone", plain)
	}
}

// The pool writes a container's keypair into its config volume as root on the
// host, and the sandbox can write that volume: a link it planted is refused or
// replaced, never followed out of it.
func TestPlaceAgentTLSFollowsNoLinkOutOfTheVolume(t *testing.T) {
	keypair := sandboxconfig.AgentTLS{Cert: "cert", Key: "key"}
	outside := t.TempDir()
	target := filepath.Join(outside, "target")
	if err := os.WriteFile(target, []byte("host file"), 0o644); err != nil {
		t.Fatal(err)
	}

	planted := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(planted, sandboxconfig.AgentTLSDirName)); err != nil {
		t.Fatal(err)
	}
	if err := placeAgentTLS(planted, keypair); err == nil {
		t.Fatal("placed the keypair through a directory link out of the volume")
	}

	staged := t.TempDir()
	dir := filepath.Join(staged, sandboxconfig.AgentTLSDirName)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{sandboxconfig.AgentTLSKeyName, sandboxconfig.AgentTLSKeyName + ".tmp"} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := placeAgentTLS(staged, keypair); err != nil {
		t.Fatalf("place the keypair over planted file links: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "host file" {
		t.Fatalf("the host file a link named = %q, %v; want it untouched", got, err)
	}
	info, err := os.Lstat(filepath.Join(dir, sandboxconfig.AgentTLSKeyName))
	if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("placed key = %v, %v; want a file of its own, root's alone", info, err)
	}
}

// A container's keypair is placed at every start where its bootstrap said
// TLS, and not where it said plain HTTP.
func TestAContainersKeypairIsPlacedWhereItServesTLS(t *testing.T) {
	r := runtimeConfigTestRuntime(t)
	dir := filepath.Join(r.sandboxConfigRoot("sbx_1"), sandboxconfig.AgentTLSDirName)
	if err := os.MkdirAll(r.sandboxConfigRoot("sbx_1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.recordAgentTLS("sbx_1", false); err != nil {
		t.Fatal(err)
	}
	if err := r.placeAgentKeypair("sbx_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("a container told plain HTTP was placed a keypair: %v", err)
	}
	if err := r.recordAgentTLS("sbx_1", true); err != nil {
		t.Fatal(err)
	}
	if err := r.placeAgentKeypair("sbx_1"); err != nil {
		t.Fatal(err)
	}
	cert, err := os.ReadFile(filepath.Join(dir, sandboxconfig.AgentTLSCertName))
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(filepath.Join(dir, sandboxconfig.AgentTLSKeyName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.X509KeyPair(cert, key); err != nil {
		t.Fatalf("the placed keypair does not load: %v", err)
	}
}
