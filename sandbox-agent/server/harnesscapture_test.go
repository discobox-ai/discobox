package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	hc "github.com/discobox-ai/discobox/pkg/harnesscapture"
)

func TestCapturePrivateScopesRequireExplicitGrant(t *testing.T) {
	for _, scope := range []string{hc.ScopeCapture, hc.ScopeWrite} {
		for _, broad := range []string{"*", "sandbox:*", "exec:*", "files:*"} {
			if (SignedTokenClaims{Scopes: []string{broad}}).HasScope(scope) {
				t.Fatalf("%s grants %s", broad, scope)
			}
		}
		if !(SignedTokenClaims{Scopes: []string{scope}}).HasScope(scope) {
			t.Fatalf("explicit %s refused", scope)
		}
		if poolOnlyScopes[scope] {
			t.Fatal("pool key may not authorize credential capture")
		}
	}
	for _, test := range []struct{ method, path, scope string }{
		{http.MethodGet, "harness-config-changes", hc.ScopeRead},
		{http.MethodPost, "harness-config-changes/discard", hc.ScopeWrite},
		{http.MethodPost, "harness-config-capture", hc.ScopeCapture},
		{http.MethodPost, "harness-config-ack", hc.ScopeWrite},
		{http.MethodPost, "harness-config-reconsider", hc.ScopeWrite},
	} {
		req := httptest.NewRequestWithContext(t.Context(), test.method, "/api/projects/p/sandboxes/s/"+test.path, nil)
		if got := requiredRequestScope(req); got != test.scope {
			t.Fatalf("%s: %q", test.path, got)
		}
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/projects/p/sandboxes/s/execs/harness-config-capture", nil)
	if got := requiredRequestScope(req); got != ScopeExecRead {
		t.Fatalf("exec name hijacked scope: %q", got)
	}
}
func TestCapturePreviewWireRoundTrip(t *testing.T) {
	for _, p := range []hc.Preview{{State: "unsupported"}, {HarnessConfigID: "cfg", ConfigRevision: 1, State: "pending", CandidateID: "random", Files: []hc.FileChange{{Path: "settings", Remove: true}}}} {
		value, err := capturePreview(p, nil)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "generation") || strings.Contains(string(data), "value") {
			t.Fatal("private fields in preview")
		}
	}
	c := hc.Candidate{ID: "candidate", Baseline: hc.Baseline{HarnessConfigID: "cfg", ConfigRevision: 1}, WriteRevision: 1, Credentials: []hc.Credential{{Provider: "test", EnvName: "TOKEN", Type: "token", Value: json.RawMessage(`{"token":"private"}`)}}}
	wire, err := captureConvert[sandboxapi.HarnessConfigCandidate](c)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := captureConvert[hc.Candidate](wire)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored.Credentials[0].Value) != `{"token":"private"}` {
		t.Fatal("private transport changed credential")
	}
}

// Exercise auth and ogen together: they must interpret escaped route literals
// identically, including percent-encoded unreserved characters.
func TestCaptureScopesMatchGeneratedRouterNormalization(t *testing.T) {
	public, sign := sandboxAgentTestSigner(t)
	auth, err := NewSignedTokenAuthenticator(Identity{ProjectID: "project-1", SandboxID: "sandbox-1"}, public, "")
	if err != nil {
		t.Fatal(err)
	}
	router, err := sandboxapi.NewServer(sandboxapi.UnimplementedHandler{})
	if err != nil {
		t.Fatal(err)
	}
	handler := auth.Middleware(router)
	for _, tc := range []struct{ method, suffix, scope, body string }{
		{http.MethodGet, "%68arness-config-changes", hc.ScopeRead, ""},
		{http.MethodPost, "%68arness-config-capture", hc.ScopeCapture, `{"candidateId":"candidate"}`},
		{http.MethodPost, "harness-config-changes/%64iscard", hc.ScopeWrite, `{"candidateId":"candidate"}`},
		{http.MethodPost, "%68arness-config-ack", hc.ScopeWrite, `{"candidateId":"candidate","baseline":{"harnessConfigId":"cfg","configRevision":1,"manifestDigest":"digest"},"files":[],"sentinels":{}}`},
		{http.MethodPost, "%68arness-config-reconsider", hc.ScopeWrite, `{"selection":{"candidateId":"candidate","keepFiles":[],"replaceCredentials":[]},"current":{"harnessConfigId":"cfg","configRevision":1,"manifestDigest":"digest"}}`},
	} {
		for _, scope := range []string{ScopeStatusRead, tc.scope} {
			req := httptest.NewRequestWithContext(t.Context(), tc.method, "http://local/api/projects/project-1/sandboxes/sandbox-1/"+tc.suffix, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+sign("project-1", "sandbox-1", "", scope))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			want := http.StatusForbidden
			if scope == tc.scope {
				want = http.StatusNotImplemented
			}
			if w.Code != want {
				t.Fatalf("%s with %s: %d, want %d", tc.suffix, scope, w.Code, want)
			}
		}
	}
}
