package secrets

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type gatedResolver struct {
	fakeResolver
	authorizeCalls int
	gate           func(AuthorizeRequest) error
}

func (r *gatedResolver) Authorize(_ context.Context, req AuthorizeRequest) error {
	r.authorizeCalls++
	return r.gate(req)
}

func TestAuthorizationRunsBeforeCachedAndPreviousCredentials(t *testing.T) {
	r := &gatedResolver{gate: func(req AuthorizeRequest) error {
		if req.Request.Method == "DELETE" {
			return ErrDenied
		}
		return nil
	}}
	r.fn = func(ResolveRequest) (ResolveResult, error) {
		return ResolveResult{Value: "REAL", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	sw := New(r, Config{Sentinels: map[string][]string{"sb": {"SENTINEL"}}})
	for _, method := range []string{"GET", "GET", "DELETE"} {
		req := newRequest(t, method, "https://api.example.com/repo")
		req.Header.Set("Authorization", "Bearer SENTINEL")
		result := sw.Apply(context.Background(), req, "sb")
		if result.Swapped() != (method != "DELETE") {
			t.Fatalf("%s swapped=%v", method, result.Swapped())
		}
		if result.RequestID == "" {
			t.Fatal("missing audit correlation")
		}
	}
	if r.calls.Load() != 1 || r.authorizeCalls != 3 {
		t.Fatalf("resolve=%d authorize=%d", r.calls.Load(), r.authorizeCalls)
	}
	req := newRequest(t, "DELETE", "https://api.example.com/repo")
	req.Header.Set("Authorization", "Bearer SENTINEL")
	if sw.ApplyPrevious(context.Background(), req, "sb").Swapped() {
		t.Fatal("retry bypassed judge")
	}
	if r.authorizeCalls != 4 {
		t.Fatal("previous value skipped authorization")
	}
}

func TestAllCredentialsAreAuthorizedBeforeAnyResolution(t *testing.T) {
	r := &gatedResolver{gate: func(req AuthorizeRequest) error {
		if len(req.Sentinels) != 2 {
			t.Fatalf("matched %v", req.Sentinels)
		}
		return ErrDenied
	}}
	r.fn = func(ResolveRequest) (ResolveResult, error) {
		t.Fatal("resolved before all credentials passed")
		return ResolveResult{}, nil
	}
	sw := New(r, Config{Sentinels: map[string][]string{"sb": {"FIRSTSENTINEL", "SECONDSENTINEL"}}, ScanQuery: true})
	req := newRequest(t, "GET", "https://api.example.com/?token=SECONDSENTINEL")
	original := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:FIRSTSENTINEL"))
	req.Header.Set("Authorization", original)
	if sw.Apply(context.Background(), req, "sb").Swapped() || req.Header.Get("Authorization") != original {
		t.Fatal("partial swap after denial")
	}
}

func TestResolutionFailureLeavesAllCredentialsUntouched(t *testing.T) {
	r := &gatedResolver{gate: func(AuthorizeRequest) error { return nil }}
	r.fn = func(req ResolveRequest) (ResolveResult, error) {
		if req.Sentinel == "SECOND" {
			return ResolveResult{}, errors.New("unavailable")
		}
		return ResolveResult{Value: "REAL"}, nil
	}
	sw := New(r, Config{Sentinels: map[string][]string{"sb": {"FIRST", "SECOND"}}})
	req := newRequest(t, "GET", "https://api.example.com/")
	req.Header.Set("Authorization", "FIRST SECOND")
	if sw.Apply(context.Background(), req, "sb").Swapped() || req.Header.Get("Authorization") != "FIRST SECOND" {
		t.Fatal("partial credential substitution")
	}
}

func TestEvidencePreservesBodyAndRedactsCredentials(t *testing.T) {
	body := `{"query":"mutation { deleteRepository(id: 42) }","token":"private"}`
	req := newRequest(t, "POST", "https://api.example.com/graphql?token=SENTINEL")
	req.Body = io.NopCloser(strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer SECRET")
	evidence, err := (AuthorizeRequest{Request: req, Sentinels: []string{"SENTINEL"}}).Evidence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(evidence, "deleteRepository") || strings.Contains(evidence, "private") || strings.Contains(evidence, "SECRET") || strings.Contains(evidence, "SENTINEL") {
		t.Fatalf("bad evidence: %s", evidence)
	}
	restored, _ := io.ReadAll(req.Body)
	if string(restored) != body {
		t.Fatalf("body changed: %s", restored)
	}
}

func TestUnsupportedAndTruncatedEvidenceFailsClosed(t *testing.T) {
	for _, tc := range []struct{ kind, body string }{{"application/octet-stream", "binary"}, {"application/json", `{} trailing`}, {"application/json", `{"operation":"read","operation":"delete"}`}, {"application/json", strings.Repeat(" ", 600<<10)}} {
		req := newRequest(t, http.MethodPost, "https://example.com")
		req.Body = io.NopCloser(strings.NewReader(tc.body))
		req.Header.Set("Content-Type", tc.kind)
		if _, err := (AuthorizeRequest{Request: req}).Evidence(context.Background()); err == nil {
			t.Fatal("unsupported body was authorized")
		}
		replay, _ := io.ReadAll(req.Body)
		if string(replay) != tc.body {
			t.Fatal("failed inspection altered body")
		}
	}
}
