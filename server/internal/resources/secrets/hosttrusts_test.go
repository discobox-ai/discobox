package secrets_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	resourcesecrets "github.com/discobox-ai/discobox/server/internal/resources/secrets"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// A cluster's API server: a leaf signed by the cluster's own self-signed CA,
// which is the shape GKE and kubeadm both present.
var (
	testLeaf = apimodel.ObservedCertificate{
		Subject: "CN=kube-apiserver", Issuer: "CN=cluster-ca",
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		SHA256: strings.Repeat("a", 64), SpkiSha256: strings.Repeat("b", 64), Pem: "leaf-pem",
	}
	testCA = apimodel.ObservedCertificate{
		Subject: "CN=cluster-ca", Issuer: "CN=cluster-ca",
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		SHA256: strings.Repeat("c", 64), SpkiSha256: strings.Repeat("d", 64), Pem: "ca-pem",
		IsCA: serverapi.NewOptBool(true), SelfSigned: serverapi.NewOptBool(true),
	}
)

func createTrustRequest(ctx context.Context, t *testing.T, svc *resourcesecrets.Service, chain ...apimodel.ObservedCertificate) *model.HostTrustRequest {
	t.Helper()
	req, err := svc.CreateSandboxTrustRequest(ctx, testPoolID, services.CreateSandboxTrustRequestBody{
		SandboxId:     testSandboxID,
		Host:          "34.70.64.109",
		Justification: serverapi.NewOptString("the user's GKE cluster"),
		Uses:          []apimodel.SecretUse{{Description: "read-only kubectl"}},
		ObservedChain: chain,
	})
	if err != nil {
		t.Fatalf("create trust request: %v", err)
	}
	return req
}

func TestTrustRequestRecordsTheAskAndTheObservedChain(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	req := createTrustRequest(ctx, t, svc, testLeaf, testCA)
	if req.Status != model.HostTrustRequestStatusPending || req.Host != "34.70.64.109:443" {
		t.Fatalf("request = %+v, want pending for the normalized endpoint", req)
	}
	if len(req.ObservedChain) != 2 || req.ObservedChain[1].SHA256 != testCA.SHA256 || !req.ObservedChain[1].IsCA {
		t.Fatalf("chain = %+v", req.ObservedChain)
	}
	if again := createTrustRequest(ctx, t, svc, testLeaf, testCA); again.ID != req.ID {
		t.Fatalf("second ask created %s, want the open request %s reused", again.ID, req.ID)
	}
}

func TestTrustRequestRefusesAnAskWithNothingToPin(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	for name, body := range map[string]services.CreateSandboxTrustRequestBody{
		"no chain": {SandboxId: testSandboxID, Host: "10.0.0.1", Uses: []apimodel.SecretUse{{Description: "x"}}},
		"no uses":  {SandboxId: testSandboxID, Host: "10.0.0.1", ObservedChain: []apimodel.ObservedCertificate{testLeaf}},
		"no host":  {SandboxId: testSandboxID, Host: "https://x/y", Uses: []apimodel.SecretUse{{Description: "x"}}, ObservedChain: []apimodel.ObservedCertificate{testLeaf}},
	} {
		if _, err := svc.CreateSandboxTrustRequest(ctx, testPoolID, body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A pool speaks only for its own sandboxes.
	body := services.CreateSandboxTrustRequestBody{SandboxId: testSandboxID, Host: "10.0.0.1", Uses: []apimodel.SecretUse{{Description: "x"}}, ObservedChain: []apimodel.ObservedCertificate{testLeaf}}
	if _, err := svc.CreateSandboxTrustRequest(ctx, "pool-other", body); err == nil {
		t.Error("another pool's ask was accepted")
	}
}

// With no pin named, the approval takes the self-signed CA the chain carries,
// and the trust carries that CA's PEM for the proxy to verify against.
func TestApprovingATrustPinsTheChainsCAByDefault(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	req := createTrustRequest(ctx, t, svc, testLeaf, testCA)

	approved, err := svc.ApproveTrustRequest(ctx, "project-1", req.ID, services.ApproveTrustRequestBody{})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if approved.Status != model.HostTrustRequestStatusApproved || approved.TrustID == "" {
		t.Fatalf("approved = %+v", approved)
	}
	status, trust, err := svc.GetSandboxTrustRequest(ctx, testPoolID, testSandboxID, req.ID)
	if err != nil || trust == nil {
		t.Fatalf("poll: %v, trust %v", err, trust)
	}
	if status.TrustID != trust.ID || trust.Pin.Kind != model.TrustPinKindCA || trust.Pin.SHA256 != testCA.SHA256 || trust.PinPEM != "ca-pem" {
		t.Fatalf("trust = %+v", trust)
	}
	if len(trust.Uses) != 1 || !strings.HasPrefix(trust.Uses[0].UseID, "use_") || trust.GrantedBy != "user-1" {
		t.Fatalf("trust uses %+v, granted by %q", trust.Uses, trust.GrantedBy)
	}
	if left := time.Until(trust.ExpiresAt); left <= 0 || left > time.Hour+time.Minute {
		t.Fatalf("trust expires in %s, want the one-hour default", left)
	}
	trusts, err := svc.ListPoolHostTrusts(ctx, testPoolID)
	if err != nil || len(trusts) != 1 || trusts[0].ID != trust.ID {
		t.Fatalf("pool trusts = %+v, %v", trusts, err)
	}
	if other, _ := svc.ListPoolHostTrusts(ctx, "pool-other"); len(other) != 0 {
		t.Fatalf("another pool reads %d trusts", len(other))
	}
}

func TestApprovingATrustPinsOnlyWhatWasObserved(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	req := createTrustRequest(ctx, t, svc, testLeaf)

	// The leaf is not a CA, so it cannot be a ca pin, and a hash nobody
	// observed is not a pin at all.
	for _, pin := range []apimodel.TrustPin{
		{Kind: serverapi.TrustPinKindCa, SHA256: testLeaf.SHA256},
		{Kind: serverapi.TrustPinKindLeafSpki, SHA256: strings.Repeat("e", 64)},
	} {
		if _, err := svc.ApproveTrustRequest(ctx, "project-1", req.ID, services.ApproveTrustRequestBody{Pin: serverapi.NewOptTrustPin(pin)}); err == nil {
			t.Errorf("pin %+v accepted", pin)
		}
	}
	approved, err := svc.ApproveTrustRequest(ctx, "project-1", req.ID, services.ApproveTrustRequestBody{
		Pin:             serverapi.NewOptTrustPin(apimodel.TrustPin{Kind: serverapi.TrustPinKindLeafSpki, SHA256: testLeaf.SpkiSha256}),
		GrantTTLSeconds: serverapi.NewOptInt64(600),
		Uses:            serverapi.NewOptNilSecretUseArray([]apimodel.SecretUse{{Description: "kubectl get pods -n web"}}),
	})
	if err != nil {
		t.Fatalf("approve leaf key: %v", err)
	}
	_, trust, _ := svc.GetSandboxTrustRequest(ctx, testPoolID, testSandboxID, approved.ID)
	if trust == nil || trust.Pin.Kind != model.TrustPinKindLeafSPKI || trust.PinPEM != "" || trust.Uses[0].Description != "kubectl get pods -n web" {
		t.Fatalf("trust = %+v", trust)
	}
	if _, err := svc.ApproveTrustRequest(ctx, "project-1", req.ID, services.ApproveTrustRequestBody{}); !isStatus(err, 409) {
		t.Fatalf("second approval error = %v, want 409", err)
	}
}

// A pin decides who the sandbox's credentials for that host are handed to, so
// a discobox answering the inbox may not approve one.
func TestATrustIsApprovedOnlyByAPerson(t *testing.T) {
	svc, _ := newAgentCredentialService(t)
	req := createTrustRequest(testPrincipalContext(), t, svc, testLeaf, testCA)
	asSandbox := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypeSandbox, SandboxID: "sbx-lead"})
	if _, err := svc.ApproveTrustRequest(asSandbox, "project-1", req.ID, services.ApproveTrustRequestBody{}); !isStatus(err, 403) {
		t.Fatalf("error = %v, want 403", err)
	}
}

// A revoked trust answers the agent's poll as denied, as a revoked grant does,
// and leaves the pool's read.
func TestARevokedTrustReadsAsDenied(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	req := createTrustRequest(ctx, t, svc, testLeaf, testCA)
	approved, err := svc.ApproveTrustRequest(ctx, "project-1", req.ID, services.ApproveTrustRequestBody{})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSandboxHostTrust(ctx, "project-1", testSandboxID, approved.TrustID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	status, trust, err := svc.GetSandboxTrustRequest(ctx, testPoolID, testSandboxID, req.ID)
	if err != nil || trust != nil || status.Status != model.HostTrustRequestStatusApproved {
		t.Fatalf("after revoke: %+v, trust %v, %v", status, trust, err)
	}
	if trusts, _ := svc.ListPoolHostTrusts(ctx, testPoolID); len(trusts) != 0 {
		t.Fatalf("pool still reads %d trusts", len(trusts))
	}
}

func TestDenyingATrustRequest(t *testing.T) {
	ctx := testPrincipalContext()
	svc, _ := newAgentCredentialService(t)
	req := createTrustRequest(ctx, t, svc, testLeaf)
	if err := svc.DenyTrustRequest(ctx, "project-1", req.ID); err != nil {
		t.Fatalf("deny: %v", err)
	}
	pending, err := svc.ListTrustRequests(ctx, "project-1", model.HostTrustRequestStatusPending)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	if err := svc.DenyTrustRequest(ctx, "project-1", req.ID); !isStatus(err, 409) {
		t.Fatalf("second deny error = %v, want 409", err)
	}
}

// A trust is its sandbox's alone, and goes when the sandbox does.
func TestDeletingTheSandboxDeletesItsTrusts(t *testing.T) {
	ctx := testPrincipalContext()
	svc, st := newAgentCredentialService(t)
	req := createTrustRequest(ctx, t, svc, testLeaf, testCA)
	if _, err := svc.ApproveTrustRequest(ctx, "project-1", req.ID, services.ApproveTrustRequestBody{}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSandbox(ctx, "project-1", testSandboxID); err != nil {
		t.Fatalf("delete sandbox: %v", err)
	}
	if trusts, _ := svc.ListPoolHostTrusts(ctx, testPoolID); len(trusts) != 0 {
		t.Fatalf("trusts outlived their sandbox: %+v", trusts)
	}
	if _, err := st.GetHostTrustRequest(ctx, "project-1", req.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("request outlived its sandbox: %v", err)
	}
}

func isStatus(err error, code int) bool {
	var status apperrors.StatusError
	return errors.As(err, &status) && status.Status == code
}
