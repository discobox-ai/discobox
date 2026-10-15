package github

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeCompare is GitHub answering for one pull request's comparison: its
// JSON, its diff, a commit's diff, and the files at the head.
type fakeCompare struct {
	// tooBig refuses the diff, as GitHub does one past what it will send.
	tooBig bool
	// failFiles is how many reads of a file fail before one is answered.
	failFiles int

	mu   sync.Mutex
	reqs []string
}

const compareJSON = `{
	"merge_base_commit": {"sha": "base000000000"},
	"ahead_by": 2, "behind_by": 1,
	"commits": [
		{"sha": "first00000000", "commit": {"message": "First\n\nbody", "author": {"name": "Alice A", "date": "2026-10-01T10:00:00Z"}}, "author": {"login": "alice"}},
		{"sha": "second0000000", "commit": {"message": "Second", "author": {"name": "Bob B", "date": "2026-10-02T10:00:00Z"}}}
	]
}`

// pullFiles is the pull request's own list of its files, in two pages.
var pullFiles = []string{
	`[{"filename": "a.go", "status": "modified", "patch": "@@ -1 +1 @@\n-old\n+new"},
	  {"filename": "gone.go", "status": "removed", "patch": "@@ -1 +0,0 @@\n-bye"}]`,
	`[{"filename": "big.bin", "status": "added"}]`,
}

const compareDiff = "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n"

func (f *fakeCompare) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept := r.Header.Get("Accept")
		f.mu.Lock()
		f.reqs = append(f.reqs, r.URL.RequestURI()+" "+accept)
		f.mu.Unlock()
		const compare = "/repos/acme/foo/compare/b1...h1"
		switch {
		case r.URL.Path == compare && accept == diffMedia:
			if f.tooBig {
				w.WriteHeader(http.StatusNotAcceptable)
				fmt.Fprint(w, `{"message": "Sorry, the diff exceeded the maximum number of files (300)."}`)
				return
			}
			fmt.Fprint(w, compareDiff)
		case r.URL.Path == compare:
			fmt.Fprint(w, compareJSON)
		case r.URL.Path == "/repos/acme/foo/pulls/9/files":
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprint(w, pullFiles[1])
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/acme/foo/pulls/9/files?per_page=100&page=2>; rel="next"`, r.Host))
			fmt.Fprint(w, pullFiles[0])
		case r.URL.Path == "/repos/acme/foo/commits/first00000000" && accept == diffMedia:
			fmt.Fprint(w, "diff --git a/a.go b/a.go\n")
		case strings.HasPrefix(r.URL.Path, "/repos/acme/foo/contents/") && accept == rawMedia:
			f.mu.Lock()
			fail := f.failFiles > 0
			f.failFiles--
			f.mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			fmt.Fprintf(w, "%s at %s\n", strings.TrimPrefix(r.URL.Path, "/repos/acme/foo/contents/"), r.URL.Query().Get("ref"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeCompare) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.reqs
	f.reqs = nil
	return out
}

// A pull request is read as its Files changed: three dots between the two
// commits it was read at, its commits newest first, and its base where it was.
func TestAPullRequestIsScannedBetweenItsCommits(t *testing.T) {
	t.Parallel()
	fake := &fakeCompare{}
	src := client(fake.serve(t), "tok").PullSource(t.Context(), "acme", "foo", 9, "b1", "h1", "main", "fix")
	scan, err := src.Scan("", 5)
	if err != nil {
		t.Fatal(err)
	}
	if scan.Diff != compareDiff || scan.Incomplete != "" {
		t.Errorf("diff %q, incomplete %q; want the comparison's whole diff", scan.Diff, scan.Incomplete)
	}
	if scan.Branch != "fix" || scan.Base.Ref != "main" || scan.Base.SHA != "base000000000" || scan.Base.Ahead != 2 || scan.Base.Behind != 1 {
		t.Errorf("branch %q, base %+v", scan.Branch, scan.Base)
	}
	var commits []string
	for _, c := range scan.Commits {
		commits = append(commits, c.Short+" "+c.Subject+" "+c.Author)
	}
	if want := []string{"second0 Second Bob B", "first00 First alice"}; !slices.Equal(commits, want) {
		t.Errorf("commits = %q, want %q", commits, want)
	}
}

// A file is read once: a commit's file never changes, and the window reads
// every changed one as each scan lands. Nothing is read before it asks.
func TestAPullRequestsFilesAreReadOnce(t *testing.T) {
	t.Parallel()
	fake := &fakeCompare{}
	src := client(fake.serve(t), "tok").PullSource(t.Context(), "acme", "foo", 9, "b1", "h1", "main", "fix")
	if _, err := src.Scan("", 5); err != nil {
		t.Fatal(err)
	}
	for _, req := range fake.requests() {
		if strings.Contains(req, "/contents/") {
			t.Errorf("a scan read a file nobody asked for: %s", req)
		}
	}
	for range 2 {
		if text, err := src.FileContent("", "a.go"); err != nil || text != "a.go at h1\n" {
			t.Errorf("the head's a.go = %q, %v", text, err)
		}
	}
	if text, _ := src.FileContent("first00000000", "dir/a b.go"); text != "dir/a b.go at first00000000\n" {
		t.Errorf("a file at a commit = %q", text)
	}
	for range 2 {
		if _, err := src.CommitDiff("first00000000", 5); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"/repos/acme/foo/contents/a.go?ref=h1 " + rawMedia,
		"/repos/acme/foo/contents/dir/a%20b.go?ref=first00000000 " + rawMedia,
		"/repos/acme/foo/commits/first00000000 " + diffMedia,
	}
	if got := fake.requests(); !slices.Equal(got, want) {
		t.Errorf("asked for %q, want each once: %q", got, want)
	}
}

// The window reads its files several at once, so the source is asked from
// more than one goroutine.
func TestAPullRequestsFilesAreReadAtOnce(t *testing.T) {
	t.Parallel()
	fake := &fakeCompare{}
	src := client(fake.serve(t), "tok").PullSource(t.Context(), "acme", "foo", 9, "b1", "h1", "main", "fix")
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			path := "f" + strconv.Itoa(i%4) + ".go"
			if text, err := src.FileContent("", path); err != nil || text != path+" at h1\n" {
				t.Errorf("%s = %q, %v", path, text, err)
			}
		})
	}
	wg.Wait()
}

// A diff too big for GitHub to send is refused, not cut; the files' own
// patches are what is left of it, and the window is told it is not all.
func TestAPullRequestTooBigForOneDiffIsPutTogether(t *testing.T) {
	t.Parallel()
	fake := &fakeCompare{tooBig: true}
	src := client(fake.serve(t), "tok").PullSource(t.Context(), "acme", "foo", 9, "b1", "h1", "main", "fix")
	scan, err := src.Scan("", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n",
		"diff --git a/gone.go b/gone.go\ndeleted file mode 100644\n--- a/gone.go\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye\n",
	} {
		if !strings.Contains(scan.Diff, want) {
			t.Errorf("the diff put together is missing %q:\n%s", want, scan.Diff)
		}
	}
	if !strings.Contains(scan.Incomplete, "1 of 3 files have none") {
		t.Errorf("incomplete = %q, want it to say a file has no patch", scan.Incomplete)
	}
}

// With no token there are sixty requests an hour, and the window reads every
// changed file as each scan lands: so with none no file is read, and the diff
// is shown without what lies around its hunks.
func TestAnAnonymousPullRequestReadsNoFiles(t *testing.T) {
	t.Parallel()
	fake := &fakeCompare{}
	src := client(fake.serve(t), "").PullSource(t.Context(), "acme", "foo", 9, "b1", "h1", "main", "fix")
	if _, err := src.Scan("", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := src.FileContent("", "a.go"); !errors.Is(err, ErrNoToken) {
		t.Errorf("a file read with no token: %v, want ErrNoToken", err)
	}
	for _, req := range fake.requests() {
		if strings.Contains(req, "/contents/") {
			t.Errorf("read a file with no token: %s", req)
		}
	}
}

// A read that failed is asked again next time: a timeout or a refusal is not
// what the file says.
func TestAFileThatFailedToReadIsReadAgain(t *testing.T) {
	t.Parallel()
	fake := &fakeCompare{failFiles: 1}
	src := client(fake.serve(t), "tok").PullSource(t.Context(), "acme", "foo", 9, "b1", "h1", "main", "fix")
	if _, err := src.FileContent("", "a.go"); err == nil {
		t.Fatal("the first read did not fail")
	}
	if text, err := src.FileContent("", "a.go"); err != nil || text != "a.go at h1\n" {
		t.Errorf("the read after a failure = %q, %v; want the file", text, err)
	}
}
