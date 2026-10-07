package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/discobox-ai/discobox/endpoint"
)

type fakeAdmission map[endpoint.IrohID]bool

func (f fakeAdmission) Authorize(_ context.Context, peer endpoint.IrohID) error {
	if f[peer] {
		return nil
	}
	return errors.New("not enrolled")
}

// A request over iroh is its enrolled peer's, checked against the enrollments
// on every request; one whose peer is no longer enrolled is refused, never
// passed on to the default user.
func TestIrohPeerAuthenticatorChecksTheEnrollmentEachRequest(t *testing.T) {
	var enrolled, revoked endpoint.IrohID
	enrolled[0], revoked[0] = 1, 2
	authn := IrohPeerAuthenticator{Admission: fakeAdmission{enrolled: true}, UserID: "user-default"}
	request := func(peer *endpoint.IrohID) *http.Request {
		ctx := context.Background()
		if peer != nil {
			ctx = WithIrohPeer(ctx, *peer)
		}
		return httptest.NewRequestWithContext(ctx, http.MethodGet, "/projects", nil)
	}

	principal, ok, err := authn.Authenticate(request(&enrolled))
	if err != nil || !ok || principal.Type != PrincipalTypeUser || principal.UserID != "user-default" || principal.IrohPeer != enrolled.String() || principal.Asserted() {
		t.Fatalf("enrolled peer: %#v, %v, %v", principal, ok, err)
	}
	if _, ok, err := authn.Authenticate(request(&revoked)); ok || err == nil {
		t.Fatalf("revoked peer: ok %v, err %v; want refused", ok, err)
	}
	if _, ok, err := (IrohPeerAuthenticator{UserID: "user-default"}).Authenticate(request(&enrolled)); ok || err == nil {
		t.Fatalf("no admission gate: ok %v, err %v; want refused", ok, err)
	}
	if _, ok, err := authn.Authenticate(request(nil)); ok || err != nil {
		t.Fatalf("not over iroh: ok %v, err %v; want it not to apply", ok, err)
	}

	// With authentication required, the enrolled peer is how the CLI gets in,
	// SSH included, and a request with no peer has nobody to be.
	chain := Authentication(DiscobotAuthenticator{}, PoolBootstrapAuthenticator{}, authn)(Authorization(SSHConnectAuthorizer{}, AuthenticatedAuthorizer{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	for _, tc := range []struct {
		name string
		path string
		peer *endpoint.IrohID
		want int
	}{
		{"enrolled peer, projects", "/projects", &enrolled, http.StatusNoContent},
		{"enrolled peer, ssh", SSHConnectPath, &enrolled, http.StatusNoContent},
		{"revoked peer", "/projects", &revoked, http.StatusUnauthorized},
		{"no peer", "/projects", nil, http.StatusUnauthorized},
	} {
		recorder := httptest.NewRecorder()
		r := request(tc.peer)
		r.URL.Path = tc.path
		chain.ServeHTTP(recorder, r)
		if recorder.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, recorder.Code, tc.want)
		}
	}
}
