package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/files"
)

// The files API's helper is this test binary, as it is the agent's binary in
// a sandbox.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == files.HelperCommand {
		os.Exit(files.RunHelper(os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

const filesTestBase = "/api/projects/project-1/sandboxes/sandbox-1"

func filesRequest(t *testing.T, router http.Handler, token, method, target string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	resp := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), method, filesTestBase+target, body)
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	router.ServeHTTP(resp, req)
	return resp
}

// Every operation reaches the files on the routes the contract names, and
// answers a refusal with the status it says.
func TestFilesAPIRoundTrip(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	token := signToken("project-1", "sandbox-1", "worker-1", ScopeFilesRead, ScopeFilesWrite)
	dir := t.TempDir()
	path := filepath.Join(dir, "notes", "a.md")
	q := func(values ...string) string {
		v := url.Values{}
		for i := 0; i < len(values); i += 2 {
			v.Set(values[i], values[i+1])
		}
		return "?" + v.Encode()
	}

	resp := filesRequest(t, router, token, http.MethodPut, "/files/content"+q("path", path, "parents", "true"), strings.NewReader("# hello\n"), "application/octet-stream")
	if resp.Code != http.StatusOK {
		t.Fatalf("write = %d %s", resp.Code, resp.Body)
	}
	var written struct {
		Path, Type, Version string
		Size                int64
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &written); err != nil || written.Path != path || written.Type != "file" || written.Size != 8 {
		t.Fatalf("written = %+v, %v (%s)", written, err, resp.Body)
	}
	if resp := filesRequest(t, router, token, http.MethodPut, "/files/content"+q("path", path, "ifExists", "fail"), strings.NewReader("x"), "application/octet-stream"); resp.Code != http.StatusPreconditionFailed {
		t.Fatalf("write that must create = %d %s, want 412", resp.Code, resp.Body)
	}
	if resp := filesRequest(t, router, token, http.MethodPut, "/files/content"+q("path", path, "ifVersion", "old"), strings.NewReader("x"), "application/octet-stream"); resp.Code != http.StatusPreconditionFailed {
		t.Fatalf("write at a stale version = %d %s, want 412", resp.Code, resp.Body)
	}
	resp = filesRequest(t, router, token, http.MethodGet, "/files/content"+q("path", path, "offset", "2", "length", "5"), nil, "")
	if resp.Code != http.StatusOK || resp.Body.String() != "hello" {
		t.Fatalf("read = %d %q", resp.Code, resp.Body)
	}
	resp = filesRequest(t, router, token, http.MethodGet, "/files/list"+q("path", filepath.Dir(path)), nil, "")
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"name":"a.md"`) {
		t.Fatalf("list = %d %s", resp.Code, resp.Body)
	}
	if resp := filesRequest(t, router, token, http.MethodGet, "/files/stat"+q("path", path+".nope"), nil, ""); resp.Code != http.StatusNotFound {
		t.Fatalf("stat of nothing = %d %s, want 404", resp.Code, resp.Body)
	}
	for _, step := range []struct{ op, body string }{
		{"copy", `{"from":"` + path + `","to":"` + path + `.bak"}`},
		{"move", `{"from":"` + path + `.bak","to":"` + filepath.Join(dir, "b.md") + `"}`},
		{"mkdir", `{"path":"` + filepath.Join(dir, "x", "y") + `","parents":true}`},
	} {
		if resp := filesRequest(t, router, token, http.MethodPost, "/files/"+step.op, strings.NewReader(step.body), "application/json"); resp.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", step.op, resp.Code, resp.Body)
		}
	}
	if resp := filesRequest(t, router, token, http.MethodPost, "/files/remove", strings.NewReader(`{"path":"`+filepath.Join(dir, "x")+`"}`), "application/json"); resp.Code != http.StatusConflict {
		t.Fatalf("remove of a full directory = %d %s, want 409", resp.Code, resp.Body)
	}
	if resp := filesRequest(t, router, token, http.MethodPost, "/files/remove", strings.NewReader(`{"path":"`+filepath.Join(dir, "x")+`","recursive":true}`), "application/json"); resp.Code != http.StatusNoContent {
		t.Fatalf("remove = %d %s", resp.Code, resp.Body)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "b.md")); err != nil || string(got) != "# hello\n" {
		t.Fatalf("moved copy = %q, %v", got, err)
	}
}

// The files API has scopes of its own: reading files does not write them, and
// running commands reads none of them; nor do the files scopes reach an exec,
// even one named "files" (ADR 26-10-22-499 §3).
func TestFilesAPIHasItsOwnScopes(t *testing.T) {
	publicKey, signToken := sandboxAgentTestSigner(t)
	router, err := NewRouter(testConfig(publicKey))
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	token := func(scopes ...string) string { return signToken("project-1", "sandbox-1", "worker-1", scopes...) }
	path := filepath.Join(t.TempDir(), "f")
	read := "/files/stat?path=" + url.QueryEscape(path)
	write := "/files/content?path=" + url.QueryEscape(path)

	if resp := filesRequest(t, router, token(ScopeExecRead, ScopeExecWrite), http.MethodGet, read, nil, ""); resp.Code != http.StatusForbidden {
		t.Errorf("stat with the exec scopes = %d, want 403", resp.Code)
	}
	if resp := filesRequest(t, router, token(ScopeFilesRead), http.MethodPut, write, strings.NewReader("x"), "application/octet-stream"); resp.Code != http.StatusForbidden {
		t.Errorf("write with files:read = %d, want 403", resp.Code)
	}
	if resp := filesRequest(t, router, token(ScopeFilesWrite), http.MethodPut, write, strings.NewReader("x"), "application/octet-stream"); resp.Code != http.StatusOK {
		t.Errorf("write with files:write = %d %s", resp.Code, resp.Body)
	}
	if resp := filesRequest(t, router, token(ScopeFilesRead), http.MethodGet, read, nil, ""); resp.Code != http.StatusOK {
		t.Errorf("stat with files:read = %d %s", resp.Code, resp.Body)
	}
	for _, target := range []string{"/execs", "/execs/files", "/services/files"} {
		if resp := filesRequest(t, router, token(ScopeFilesRead, ScopeFilesWrite), http.MethodGet, target, nil, ""); resp.Code != http.StatusForbidden {
			t.Errorf("GET %s with the files scopes = %d, want 403", target, resp.Code)
		}
	}
}
