package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/tui"
)

const trustRequestJSON = `{"id":"treq_0123456789abcdef","projectId":"project-1","sandboxId":"sbx_1","requestedBy":"agent:sbx_1","host":"34.70.64.109:443","status":"%s",` +
	`"uses":[{"description":"read-only kubectl"}],"observedChain":[` +
	`{"subject":"CN=kube-apiserver","issuer":"CN=cluster-ca","ips":["34.70.64.109"],"notBefore":"2026-09-01T00:00:00Z","notAfter":"2027-09-01T00:00:00Z","sha256":"aaaa","spkiSha256":"bbbb","pem":"leaf"},` +
	`{"subject":"CN=cluster-ca","issuer":"CN=cluster-ca","notBefore":"2026-01-01T00:00:00Z","notAfter":"2031-01-01T00:00:00Z","sha256":"cccc","spkiSha256":"dddd","isCA":true,"selfSigned":true,"pem":"ca"}],` +
	`"createdAt":"2026-09-24T00:00:00Z","updatedAt":"2026-09-24T00:00:01Z"}`

// The pin is typed as `trust request get` prints it, and the lifetime as it
// is said.
func TestTrustRequestApproveSendsThePinAndLifetime(t *testing.T) {
	var approved map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/trust-requests/treq_0123456789abcdef/approve":
			if err := json.NewDecoder(r.Body).Decode(&approved); err != nil {
				t.Fatalf("decode approve body: %v", err)
			}
			_, _ = w.Write([]byte(strings.Replace(trustRequestJSON, "%s", "approved", 1)))
		default:
			t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "trust", "request", "approve", "treq_0123456789abcdef", "--pin", "ca:CCCC", "--ttl", "1d"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute trust request approve: %v", err)
	}
	pin, _ := approved["pin"].(map[string]any)
	if pin["kind"] != "ca" || pin["sha256"] != "cccc" || approved["grantTTLSeconds"] != float64(86400) {
		t.Fatalf("approve body = %#v", approved)
	}
	if !strings.Contains(out.String(), "approved") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestTrustRequestApproveRefusesForever(t *testing.T) {
	cmd := NewRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--server", "http://127.0.0.1:1", "--project", "project-1", "trust", "request", "approve", "treq_0123456789abcdef", "--ttl", "forever"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "always lapses") {
		t.Fatalf("error = %v, want forever refused", err)
	}
}

// What a pin is decided on is printed with the value --pin takes for it.
func TestTrustRequestGetShowsTheChainAndItsPins(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || r.URL.Path != "/projects/project-1/trust-requests/treq_0123456789abcdef" {
			t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(strings.Replace(trustRequestJSON, "%s", "pending", 1)))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "trust", "request", "get", "treq_0123456789abcdef"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute trust request get: %v", err)
	}
	for _, want := range []string{"34.70.64.109:443", "read-only kubectl", "CN=kube-apiserver", "leaf-spki:bbbb", "ca:cccc"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output = %q, want %q", out.String(), want)
		}
	}
}

// The inbox carries a trust item whole, and offers first the pin the server
// would take without one.
func TestATrustItemBecomesAnInboxRequestWithItsDefaultPin(t *testing.T) {
	var request apimodel.HostTrustRequest
	if err := json.Unmarshal([]byte(strings.Replace(trustRequestJSON, "%s", "pending", 1)), &request); err != nil {
		t.Fatalf("decode: %v", err)
	}
	req := toTUITrustRequest(request)
	if req.Trust == nil || req.Host != "34.70.64.109:443" || req.SandboxID != "sbx_1" || len(req.Uses) != 1 || len(req.Trust.Chain) != 2 {
		t.Fatalf("request = %+v", req)
	}
	if req.Trust.DefaultPin != (tui.TrustPin{Kind: tui.TrustPinCA, SHA256: "cccc"}) {
		t.Fatalf("default pin = %+v, want the chain's self-signed CA", req.Trust.DefaultPin)
	}
	if names := req.Trust.Chain[0].Names; len(names) != 1 || names[0] != "34.70.64.109" {
		t.Fatalf("leaf names = %v", names)
	}
}
