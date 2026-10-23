package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// execServer answers the exec API for sbx_1: every exec created runs at once,
// and answer says what its command printed and exited with. created records
// each one's command.
func execServer(t *testing.T, answer func(command []string) (stdout string, code int64)) (*apiDataSource, func() [][]string) {
	t.Helper()
	var mu sync.Mutex
	var created [][]string
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Errorf("encoding an answer: %v", err)
		}
	}
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		const execs = "/api/projects/project-1/sandboxes/sbx_1/execs"
		rest, isExec := strings.CutPrefix(r.URL.Path, execs+"/exec_")
		id, tail, _ := strings.Cut(rest, "/")
		n, _ := strconv.Atoi(id)
		mu.Lock()
		var command []string
		if isExec && n >= 1 && n <= len(created) {
			command = created[n-1]
		}
		mu.Unlock()
		record := func(status string) map[string]any {
			rec := map[string]any{"id": "exec_" + id, "status": status, "command": command, "createdAt": "2026-10-23T09:00:00Z"}
			if status == "exited" {
				_, code := answer(command)
				rec["exitCode"] = code
			}
			return rec
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == execs:
			var body struct {
				Command []string `json:"command"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			created = append(created, body.Command)
			id = strconv.Itoa(len(created))
			mu.Unlock()
			write(w, map[string]any{"exec": map[string]any{"id": "exec_" + id, "status": "created", "command": body.Command, "createdAt": "2026-10-23T09:00:00Z"}})
		case command == nil:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && tail == "start":
			write(w, record("running"))
		case r.Method == http.MethodGet && tail == "":
			write(w, record("exited"))
		case r.Method == http.MethodGet && tail == "logs":
			stdout, _ := answer(command)
			write(w, map[string]any{"entries": []map[string]any{{"timestamp": "2026-10-23T09:00:01Z", "stream": "stdout", "data": []byte(stdout)}}})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	client, err := app.apiClient()
	if err != nil {
		t.Fatalf("api client: %v", err)
	}
	return &apiDataSource{app: app, client: client, projectID: "project-1"}, func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return append([][]string(nil), created...)
	}
}

// onBranch answers the branch check with branch, and the head check with
// whether the head is there.
func onBranch(branch string, hasHead bool) func([]string) (string, int64) {
	return func(command []string) (string, int64) {
		switch {
		case len(command) > 1 && command[1] == "symbolic-ref":
			return branch + "\n", 0
		case hasHead:
			return "", 0
		}
		return "", 1
	}
}

// A head the discobox does not have is said, and discobox-review is not
// started over it: it would exit at once, and the tab could not say why.
func TestPullReviewSaysAHeadThatIsNotThere(t *testing.T) {
	ds, created := execServer(t, onBranch("fix-reaper", false))
	pull := tui.PullRequest{Head: "fix-reaper", HeadRef: "fix-reaper", Base: "main", BaseSHA: strings.Repeat("a", 40), HeadSHA: "0123456789ab" + strings.Repeat("c", 28)}
	_, _, err := ds.PullReview(t.Context(), "sbx_1", pull, 80, 24)
	if err == nil || !strings.Contains(err.Error(), "0123456789ab is not in this discobox") {
		t.Fatalf("err = %v, want it to say the head is not there", err)
	}
	got := created()
	if len(got) != 2 || strings.Join(got[1], " ") != "git cat-file -e "+pull.HeadSHA+"^{commit}" {
		t.Errorf("execs created: %q, want the branch and the head looked for, and nothing started", got)
	}
}

// A fork's head is labeled owner:branch on the page; the discobox working on
// it has the branch alone checked out, and that is the pull request's.
func TestPullReviewTakesAForksBranch(t *testing.T) {
	ds, created := execServer(t, onBranch("fix-reaper", false))
	pull := tui.PullRequest{Head: "someone:fix-reaper", HeadRef: "fix-reaper", Base: "main", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("c", 40)}
	_, _, err := ds.PullReview(t.Context(), "sbx_1", pull, 80, 24)
	if err == nil || !strings.Contains(err.Error(), "is not in this discobox") || len(created()) != 2 {
		t.Errorf("err = %v after %d execs, want the branch taken and the head looked for", err, len(created()))
	}
}

// The review shown is the branch checked out's, so a discobox on another
// branch, or on none, is refused before anything is started.
func TestPullReviewSaysTheDiscoboxIsOnAnotherBranch(t *testing.T) {
	pull := tui.PullRequest{Head: "fix-reaper", HeadRef: "fix-reaper", Base: "main", BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("c", 40)}
	for branch, want := range map[string]string{
		"other-work": "has other-work checked out, not the pull request's fix-reaper",
		"":           "has no branch checked out",
	} {
		ds, created := execServer(t, onBranch(branch, true))
		_, _, err := ds.PullReview(t.Context(), "sbx_1", pull, 80, 24)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("on %q: err = %v, want %q", branch, err, want)
		}
		if n := len(created()); n != 1 {
			t.Errorf("on %q: %d execs created, want only the branch read", branch, n)
		}
	}
}

// The base branch goes along as the fallback only when it can be read as one.
func TestPullBaseFallback(t *testing.T) {
	for branch, want := range map[string]string{
		"main":        "origin/main",
		"release/1.2": "origin/release/1.2",
		"":            "",
		"-x":          "",
		"a..b":        "",
		"bad name":    "",
		"x\x1b":       "",
		"tab\there":   "",
		"weird^":      "",
	} {
		if got := pullBaseFallback(branch); got != want {
			t.Errorf("pullBaseFallback(%q) = %q, want %q", branch, got, want)
		}
	}
}
