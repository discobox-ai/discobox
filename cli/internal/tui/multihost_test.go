package tui

import "testing"

// A request is shown with every route it asks for, and a trust with its
// endpoint alone.
func TestARequestIsShownWithEveryRouteItAsksFor(t *testing.T) {
	for _, tc := range []struct {
		routes []SecretRoute
		where  string
	}{
		{[]SecretRoute{{Host: "api.github.com"}}, "api.github.com"},
		{[]SecretRoute{{Host: "api.github.com"}, {Host: "uploads.github.com", Path: "/repos"}}, "api.github.com, uploads.github.com/repos"},
		{[]SecretRoute{{Host: "*"}}, "*"},
	} {
		if got := (CredentialRequest{Routes: tc.routes}).where(); got != tc.where {
			t.Errorf("where(%v) = %q, want %q", tc.routes, got, tc.where)
		}
	}
	if got := (CredentialRequest{Host: "kube.internal:6443", Trust: &TrustAsk{}}).where(); got != "kube.internal:6443" {
		t.Errorf("a trust's where = %q", got)
	}
}

// The secret named what the request asks for is offered first; every secret
// is offered, since the binding an approval makes is held to the secret's
// routes and the request's both.
func TestThePickerOffersTheSecretNamedAsAskedFirst(t *testing.T) {
	req := CredentialRequest{Name: "github", Routes: []SecretRoute{{Host: "api.github.com"}}}
	other := Secret{ID: "sec-other", Name: "openai"}
	named := Secret{ID: "sec-named", Name: "github"}
	got := secretsForRequest([]Secret{other, named}, req)
	if len(got) != 2 || got[0].ID != named.ID {
		t.Fatalf("picker order = %v; want the secret named github first, and both offered", got)
	}
}
