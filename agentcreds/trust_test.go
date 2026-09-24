package agentcreds_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
)

func TestTrustRequestRoundTrips(t *testing.T) {
	svc := &fakeService{trustStatus: agentcreds.TrustRequestStatus{
		RequestID:     "treq-1",
		Status:        agentcreds.StatusPending,
		Host:          "34.70.64.109:443",
		ObservedChain: []agentcreds.Certificate{{Subject: "CN=34.70.64.109", SHA256: "ab", SPKISHA256: "cd"}},
	}}
	client := newTestClient(t, svc)
	status, err := client.RequestTrust(context.Background(), agentcreds.TrustRequestBody{
		Host: "34.70.64.109",
		Uses: []agentcreds.RequestedUse{{Description: "read-only kubectl"}},
	})
	if err != nil {
		t.Fatalf("request trust: %v", err)
	}
	if status.RequestID != "treq-1" || len(status.ObservedChain) != 1 {
		t.Fatalf("status = %#v", status)
	}
	if svc.gotTrust.Host != "34.70.64.109" || len(svc.gotTrust.Uses) != 1 {
		t.Fatalf("service saw %#v", svc.gotTrust)
	}
}

// A pending ask is 202, as a credential ask is. One that settled on the spot
// because it needed nobody is an answer, and is 200.
func TestTrustRequestStatusCode(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   int
	}{
		{agentcreds.StatusPending, http.StatusAccepted},
		{agentcreds.StatusUnneeded, http.StatusOK},
	} {
		svc := &fakeService{trustStatus: agentcreds.TrustRequestStatus{RequestID: "treq-1", Status: tc.status}}
		server := httptest.NewServer(agentcreds.NewHandler(svc))
		body, err := json.Marshal(agentcreds.TrustRequestBody{Host: "example.internal"})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL+agentcreds.PathTrustRequests, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		server.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("%s: status %d, want %d", tc.status, resp.StatusCode, tc.want)
		}
	}
}

func TestTrustsList(t *testing.T) {
	expiry := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	svc := &fakeService{trusts: []agentcreds.Trust{{
		Host: "34.70.64.109:443",
		Pin:  agentcreds.Pin{Kind: agentcreds.PinCA, SHA256: "ab"},
		Uses: []agentcreds.Use{{UseID: "use-1", Description: "read-only kubectl", ExpiresAt: &expiry}},
	}}}
	trusts, err := newTestClient(t, svc).Trusts(context.Background())
	if err != nil {
		t.Fatalf("trusts: %v", err)
	}
	if len(trusts) != 1 || trusts[0].Pin.Kind != agentcreds.PinCA {
		t.Fatalf("trusts = %#v", trusts)
	}
}

func TestTrustRequestSettled(t *testing.T) {
	for status, want := range map[string]bool{
		agentcreds.StatusPending:  false,
		agentcreds.StatusGranted:  true,
		agentcreds.StatusDenied:   true,
		agentcreds.StatusUnneeded: true,
	} {
		if got := (agentcreds.TrustRequestStatus{Status: status}).Settled(); got != want {
			t.Errorf("Settled(%s) = %v, want %v", status, got, want)
		}
	}
}

// A trust covers one endpoint, so every spelling of it has to be one string.
func TestTrustHost(t *testing.T) {
	for in, want := range map[string]string{
		"34.70.64.109":               "34.70.64.109:443",
		"34.70.64.109:443":           "34.70.64.109:443",
		" Example.Internal:6445 ":    "example.internal:6445",
		"https://example.internal/":  "example.internal:443",
		"https://192.168.1.161:6445": "192.168.1.161:6445",
		"::1":                        "[::1]:443",
		"[::1]:6443":                 "[::1]:6443",
		"":                           "",
		"example.internal:":          "",
		"example.internal/path":      "",
		"user@example.internal":      "",
	} {
		if got := agentcreds.TrustHost(in); got != want {
			t.Errorf("TrustHost(%q) = %q, want %q", in, got, want)
		}
	}
}
