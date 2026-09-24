package access

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/agentcreds"
)

func TestTrustSendsHostUsesAndCA(t *testing.T) {
	svc := &fakeService{trustStatus: agentcreds.TrustRequestStatus{RequestID: "treq_1", Status: agentcreds.StatusPending, Host: "34.70.64.109:443"}}
	serve(t, svc)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, []byte("-----BEGIN CERTIFICATE-----\nstand-in\n-----END CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := capture(t, "", func() int {
		return Run([]string{"trust", "--use", "read-only kubectl", "34.70.64.109", "--why", "GKE", "--ca-file", ca})
	})
	if code != exitOK || !strings.HasPrefix(stdout, "treq_1 34.70.64.109:443 pending") {
		t.Fatalf("exit %d, stdout %q, stderr %s", code, stdout, stderr)
	}
	got := svc.gotTrust
	if got.Host != "34.70.64.109" || got.Justification != "GKE" || len(got.Uses) != 1 || !strings.Contains(got.SuppliedCA, "stand-in") {
		t.Fatalf("service saw %#v", got)
	}
}

func TestTrustJSONBodyAndWait(t *testing.T) {
	chain := []agentcreds.Certificate{{Subject: "CN=kube", SHA256: "ab"}}
	svc := &fakeService{
		trustStatus: agentcreds.TrustRequestStatus{RequestID: "treq_1", Status: agentcreds.StatusPending, Host: "10.0.0.1:6443", ObservedChain: chain},
		trustPolls: []agentcreds.TrustRequestStatus{
			{RequestID: "treq_1", Status: agentcreds.StatusPending},
			{RequestID: "treq_1", Status: agentcreds.StatusGranted, Pin: &agentcreds.Pin{Kind: agentcreds.PinCA, SHA256: "ab"},
				Uses: []agentcreds.Use{{UseID: "use_1", Description: "get pods"}}},
		},
	}
	serve(t, svc)
	body := `{"host":"10.0.0.1:6443","justification":"the user's cluster","uses":[{"description":"get pods"}],"wait":true,"timeoutSeconds":30}`
	stdout, stderr, code := capture(t, body, func() int { return Run([]string{"trust", "--json"}) })
	if code != exitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	var status agentcreds.TrustRequestStatus
	if err := json.Unmarshal([]byte(stdout), &status); err != nil {
		t.Fatalf("stdout %q: %v", stdout, err)
	}
	if status.Status != agentcreds.StatusGranted || status.Pin == nil || len(status.ObservedChain) != 1 {
		t.Fatalf("status = %#v, want granted with the chain it was asked with", status)
	}
	if svc.gotTrust.Justification != "the user's cluster" {
		t.Fatalf("justification = %q", svc.gotTrust.Justification)
	}
}

func TestTrustDeniedExitsNonZero(t *testing.T) {
	svc := &fakeService{
		trustStatus: agentcreds.TrustRequestStatus{RequestID: "treq_1", Status: agentcreds.StatusPending, Host: "10.0.0.1:443"},
		trustPolls:  []agentcreds.TrustRequestStatus{{RequestID: "treq_1", Status: agentcreds.StatusDenied}},
	}
	serve(t, svc)
	if _, _, code := capture(t, "", func() int { return Run([]string{"trust", "10.0.0.1", "--use", "x", "--wait"}) }); code != exitError {
		t.Fatalf("exit %d, want %d", code, exitError)
	}
}

func TestTrustRefusesANonHost(t *testing.T) {
	serve(t, &fakeService{})
	_, stderr, code := capture(t, "", func() int { return Run([]string{"trust", "https://example.internal/path", "--use", "x", "--json"}) })
	if code != exitUsage || !strings.Contains(stderr, `"code":"invalid"`) {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
}

func TestTrustsLists(t *testing.T) {
	serve(t, &fakeService{trusts: []agentcreds.Trust{{
		Host: "10.0.0.1:6443",
		Pin:  agentcreds.Pin{Kind: agentcreds.PinLeafSPKI, SHA256: "cd"},
		Uses: []agentcreds.Use{{UseID: "use_1", Description: "get pods"}},
	}}})
	stdout, _, code := capture(t, "", func() int { return Run([]string{"trusts"}) })
	if code != exitOK || !strings.Contains(stdout, "10.0.0.1:6443 (leaf-spki cd)") || !strings.Contains(stdout, "use_1  get pods") {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
}
