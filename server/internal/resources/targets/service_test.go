package targets

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/policy"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/publicnet"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/discobox/server/internal/targetdb"
)

func TestTheBuiltinEntriesAreEntries(t *testing.T) {
	entries := builtins()
	if err := checkBuiltins(entries); err != nil {
		t.Fatal(err)
	}
	var hosts []string
	for _, b := range entries {
		hosts = append(hosts, b.entry.Host)
	}
	if strings.Join(hosts, ",") != "github.com,api.discobox.internal" {
		t.Fatalf("built-in hosts = %v", hosts)
	}
}

// The pinned digests are what the pinned URLs answer. It reaches GitHub, so
// it runs only when asked to.
func TestTheBuiltinURLsAnswerTheirDigests(t *testing.T) {
	if os.Getenv("DISCOBOX_TEST_TARGET_FETCH") == "" {
		t.Skip("set DISCOBOX_TEST_TARGET_FETCH=1 to fetch the built-in documents")
	}
	cache := newDocumentCache(t.TempDir(), nil)
	for _, b := range builtins() {
		for _, d := range b.entry.Documents {
			if d.URL == "" {
				continue
			}
			f, err := cache.open(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
		}
	}
}

const restText = `{"openapi": "3.0.3", "paths": {}}`

type fixture struct {
	svc     *Service
	fetches *atomic.Int32
	cache   string
	url     string
}

// newFixture serves an https origin with one document on it, and a target
// database whose one built-in, example.com, names it.
func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	// The origin listens on loopback, which a fetch otherwise refuses.
	previous := publicnet.Internal
	publicnet.Internal = func(netip.Addr) bool { return false }
	t.Cleanup(func() { publicnet.Internal = previous })
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	for _, id := range []string{"project-1", "project-2"} {
		if err := db.Write.WithContext(ctx).Create(&model.Project{ID: id, OwnerUserID: "user-1", Name: id}).Error; err != nil {
			t.Fatalf("create project: %v", err)
		}
	}

	var fetches atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest.json":
			fetches.Add(1)
			_, _ = io.WriteString(w, restText)
		case "/wrong.json":
			_, _ = io.WriteString(w, "not what was pinned")
		case "/moved.json":
			http.Redirect(w, r, "/rest.json", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(origin.Close)

	entries := []builtin{{
		entry: targetdb.Entry{
			Host: "example.com",
			Documents: []targetdb.Document{
				{Name: "rest", Kind: targetdb.OpenAPI, URL: origin.URL + "/rest.json", SHA256: digest([]byte(restText))},
				{Name: "notes", Kind: targetdb.Prose, SHA256: digest([]byte("Notes."))},
			},
		},
		texts: map[string][]byte{"notes": []byte("Notes.")},
	}}
	if err := checkBuiltins(entries); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(t.TempDir(), "targets")
	return fixture{
		svc:     &Service{store: store.New(db.Write, db.Read), builtins: entries, documents: newDocumentCache(cache, origin.Client())},
		fetches: &fetches,
		cache:   cache,
		url:     origin.URL,
	}
}

func person() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypeUser, UserID: "user-1"})
}

func statusOf(err error) int {
	var status interface{ StatusCode() int }
	if errors.As(err, &status) {
		return status.StatusCode()
	}
	return 0
}

// document reads the text of one of an entry's documents.
func (f fixture) document(ctx context.Context, t *testing.T, projectID, host, name string) string {
	t.Helper()
	rc, err := f.svc.GetProjectTargetDocument(ctx, projectID, host, name)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func put(documents []apigen.PutTargetDocument, terms []apigen.TargetTerm, statements []apigen.TargetStatementShape) services.PutProjectTargetBody {
	return services.PutProjectTargetBody{Documents: documents, Terms: terms, Statements: statements}
}

func textDocument(name, text string) apigen.PutTargetDocument {
	return apigen.PutTargetDocument{Name: name, Kind: apigen.PutTargetDocumentKindProse, Text: apigen.NewOptString(text)}
}

func TestAProjectSeesTheBuiltinsAndReplacesOne(t *testing.T) {
	f := newFixture(t)
	ctx := person()

	list, err := f.svc.ListProjectTargets(ctx, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Host != "example.com" || !list[0].Builtin || list[0].ReplacesBuiltin {
		t.Fatalf("list = %+v", list)
	}
	builtinVersion := list[0].Version
	if builtinVersion != f.svc.builtins[0].entry.Version() {
		t.Fatalf("version = %s", builtinVersion)
	}

	terms := []apigen.TargetTerm{
		{Word: "widget", Kind: apigen.TargetTermKindType, Definition: "A widget."},
		{Word: "shop", Kind: apigen.TargetTermKindType, Definition: "A shop that sells widgets."},
	}
	shapes := []apigen.TargetStatementShape{{Action: "create", ResourceType: "widget", ScopeType: "shop", Operations: []string{"POST /widgets"}}}
	own, err := f.svc.PutProjectTarget(ctx, "project-1", "example.com", put([]apigen.PutTargetDocument{textDocument("notes", "Our own notes.")}, terms, shapes))
	if err != nil {
		t.Fatal(err)
	}
	if own.Builtin || !own.ReplacesBuiltin || own.UpdatedBy != "user-1" || own.Version == builtinVersion {
		t.Fatalf("own = %+v", own)
	}
	if own.Documents[0].SHA256 != digest([]byte("Our own notes.")) {
		t.Fatalf("documents = %+v", own.Documents)
	}
	if got := f.document(ctx, t, "project-1", "example.com", "notes"); got != "Our own notes." {
		t.Fatalf("notes = %q", got)
	}
	// The other project still sees the built-in.
	if got := f.document(ctx, t, "project-2", "example.com", "notes"); got != "Notes." {
		t.Fatalf("project-2 notes = %q", got)
	}

	// A second put replaces the first, text and all.
	again, err := f.svc.PutProjectTarget(ctx, "project-1", "example.com", put([]apigen.PutTargetDocument{textDocument("readme", "Read me.")}, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !again.CreatedAt.Equal(own.CreatedAt) || len(again.Documents) != 1 || len(again.Terms) != 0 {
		t.Fatalf("again = %+v", again)
	}
	if _, err := f.svc.GetProjectTargetDocument(ctx, "project-1", "example.com", "notes"); statusOf(err) != http.StatusNotFound {
		t.Fatalf("replaced document = %v, want 404", err)
	}

	if err := f.svc.DeleteProjectTarget(ctx, "project-1", "example.com"); err != nil {
		t.Fatal(err)
	}
	back, err := f.svc.GetProjectTarget(ctx, "project-1", "example.com")
	if err != nil || !back.Builtin || back.Version != builtinVersion {
		t.Fatalf("after delete = %+v, %v", back, err)
	}
	if err := f.svc.DeleteProjectTarget(ctx, "project-1", "example.com"); statusOf(err) != http.StatusNotFound || !strings.Contains(err.Error(), "built-in entry cannot be removed") {
		t.Fatalf("deleting the built-in = %v", err)
	}
}

func TestAProjectAddsAnEntryOfItsOwn(t *testing.T) {
	f := newFixture(t)
	ctx := person()
	doc := apigen.PutTargetDocument{Name: "rest", Kind: apigen.PutTargetDocumentKindOpenapi, URL: apigen.NewOptString(f.url + "/rest.json"), SHA256: apigen.NewOptString(digest([]byte(restText)))}
	own, err := f.svc.PutProjectTarget(ctx, "project-1", "api.acme.dev", put([]apigen.PutTargetDocument{doc}, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if own.Builtin || own.ReplacesBuiltin {
		t.Fatalf("own = %+v", own)
	}
	list, err := f.svc.ListProjectTargets(ctx, "project-1")
	if err != nil || len(list) != 2 || list[0].Host != "api.acme.dev" || list[1].Host != "example.com" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if got := f.document(ctx, t, "project-1", "api.acme.dev", "rest"); got != restText {
		t.Fatalf("rest = %q", got)
	}
	if _, err := f.svc.GetProjectTarget(ctx, "project-2", "api.acme.dev"); statusOf(err) != http.StatusNotFound {
		t.Fatalf("another project's entry = %v, want 404", err)
	}
}

func TestPutRefuses(t *testing.T) {
	f := newFixture(t)
	ctx := person()
	sum := digest([]byte(restText))
	for name, tc := range map[string]struct {
		host string
		body services.PutProjectTargetBody
		want string
	}{
		"a host with a port": {"example.com:443", put(nil, nil, nil), "without a port"},
		"a host in capitals": {"Example.com", put(nil, nil, nil), "not lowercase"},
		"a document with neither URL nor text": {"example.com", put([]apigen.PutTargetDocument{{Name: "a", Kind: apigen.PutTargetDocumentKindProse}}, nil, nil),
			"neither a URL nor text"},
		"a document with both": {"example.com", put([]apigen.PutTargetDocument{{Name: "a", Kind: apigen.PutTargetDocumentKindProse, URL: apigen.NewOptString("https://example.com/a"), SHA256: apigen.NewOptString(sum), Text: apigen.NewOptString("a")}}, nil, nil),
			"has a URL and text"},
		"a URL with no digest": {"example.com", put([]apigen.PutTargetDocument{{Name: "a", Kind: apigen.PutTargetDocumentKindProse, URL: apigen.NewOptString("https://example.com/a")}}, nil, nil),
			"is not a lowercase hex SHA-256"},
		"a URL that is not https": {"example.com", put([]apigen.PutTargetDocument{{Name: "a", Kind: apigen.PutTargetDocumentKindProse, URL: apigen.NewOptString("http://example.com/a"), SHA256: apigen.NewOptString(sum)}}, nil, nil),
			"not an https URL"},
		"text that is not its digest": {"example.com", put([]apigen.PutTargetDocument{{Name: "a", Kind: apigen.PutTargetDocumentKindProse, SHA256: apigen.NewOptString(sum), Text: apigen.NewOptString("a")}}, nil, nil),
			"has digest"},
		"a statement with a word the entry lacks": {"example.com", put(nil, nil, []apigen.TargetStatementShape{{Action: "create", ResourceType: "widget", ScopeType: "shop", Operations: []string{"POST /widgets"}}}),
			`"widget" is not one of the entry's types`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.svc.PutProjectTarget(ctx, "project-1", tc.host, tc.body)
			if statusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("put = %v, want a 400 containing %q", err, tc.want)
			}
		})
	}
}

func TestADiscoboxIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypeSandbox})
	if _, err := f.svc.ListProjectTargets(ctx, "project-1"); statusOf(err) != http.StatusForbidden {
		t.Fatalf("list = %v, want 403", err)
	}
	if _, err := f.svc.PutProjectTarget(ctx, "project-1", "example.com", put(nil, nil, nil)); statusOf(err) != http.StatusForbidden {
		t.Fatalf("put = %v, want 403", err)
	}
	if _, err := f.svc.GetProjectTargetDocument(ctx, "project-1", "example.com", "notes"); statusOf(err) != http.StatusForbidden {
		t.Fatalf("document = %v, want 403", err)
	}
}

func TestADocumentIsFetchedOnceAndChecked(t *testing.T) {
	f := newFixture(t)
	ctx := person()
	for range 3 {
		if got := f.document(ctx, t, "project-1", "example.com", "rest"); got != restText {
			t.Fatalf("rest = %q", got)
		}
	}
	if n := f.fetches.Load(); n != 1 {
		t.Fatalf("fetched %d times, want once", n)
	}

	// A cache file that is not its digest's text is never answered: a new
	// process checks it, throws it away and fetches again.
	path := filepath.Join(f.cache, digest([]byte(restText)))
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.svc.documents = newDocumentCache(f.cache, f.svc.documents.client)
	if got := f.document(ctx, t, "project-1", "example.com", "rest"); got != restText {
		t.Fatalf("rest after tampering = %q", got)
	}
	if n := f.fetches.Load(); n != 2 {
		t.Fatalf("fetched %d times, want twice", n)
	}

	// Embedded text needs no fetch.
	if got := f.document(ctx, t, "project-1", "example.com", "notes"); got != "Notes." {
		t.Fatalf("notes = %q", got)
	}
}

func TestAFetchThatIsNotTheDigestIsRefused(t *testing.T) {
	f := newFixture(t)
	ctx := person()
	doc := apigen.PutTargetDocument{Name: "rest", Kind: apigen.PutTargetDocumentKindOpenapi, URL: apigen.NewOptString(f.url + "/wrong.json"), SHA256: apigen.NewOptString(digest([]byte(restText)))}
	if _, err := f.svc.PutProjectTarget(ctx, "project-1", "api.acme.dev", put([]apigen.PutTargetDocument{doc}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.GetProjectTargetDocument(ctx, "project-1", "api.acme.dev", "rest")
	if statusOf(err) != http.StatusBadGateway || !strings.Contains(err.Error(), "did not answer the text of digest") {
		t.Fatalf("document = %v, want a 502 naming the digest", err)
	}
	entries, _ := os.ReadDir(f.cache)
	if len(entries) != 0 {
		t.Fatalf("the cache kept %v", entries)
	}
}

func TestAServerWithNoCacheFetchesNothing(t *testing.T) {
	f := newFixture(t)
	f.svc.documents = newDocumentCache("", f.svc.documents.client)
	if _, err := f.svc.GetProjectTargetDocument(person(), "project-1", "example.com", "rest"); statusOf(err) != http.StatusBadGateway {
		t.Fatalf("document = %v, want 502", err)
	}
	if f.fetches.Load() != 0 {
		t.Fatal("fetched with no cache")
	}
}

func TestLookupAnswersFromTheNarrowestEntry(t *testing.T) {
	f := newFixture(t)
	ctx := person()
	terms := []apigen.TargetTerm{
		{Word: "widget", Kind: apigen.TargetTermKindType, Definition: "A widget."},
		{Word: "shop", Kind: apigen.TargetTermKindType, Definition: "A shop that sells widgets."},
	}
	shapes := []apigen.TargetStatementShape{{Action: "create", ResourceType: "widget", ScopeType: "shop", Operations: []string{"POST /widgets"}}}
	if _, err := f.svc.PutProjectTarget(ctx, "project-1", "api.example.com", put([]apigen.PutTargetDocument{textDocument("notes", "Widgets.")}, terms, shapes)); err != nil {
		t.Fatal(err)
	}
	statement := policy.Statement{Effect: policy.Allow, Actions: []string{"create", "delete"}, ResourceType: "widget", Scope: policy.Scope{Type: "shop", Name: "main"}}

	entry, lookup, err := f.svc.LookupProjectTarget(ctx, "project-1", "API.example.com:443", statement)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Host != "api.example.com" || len(lookup.Matches) != 1 || lookup.Matches[0].Action != "create" || strings.Join(lookup.Missing, ",") != "delete" {
		t.Fatalf("lookup = %s %+v", entry.Host, lookup)
	}

	entry, lookup, err = f.svc.LookupProjectTarget(ctx, "project-1", "www.example.com", statement)
	if err != nil || entry.Host != "example.com" || !entry.Builtin || len(lookup.Missing) != 2 {
		t.Fatalf("lookup = %+v %+v %v", entry, lookup, err)
	}

	if _, _, err := f.svc.LookupProjectTarget(ctx, "project-1", "example.org", statement); statusOf(err) != http.StatusNotFound {
		t.Fatalf("lookup of an unknown host = %v, want 404", err)
	}
}

func TestDeletingAProjectTakesItsEntries(t *testing.T) {
	f := newFixture(t)
	ctx := person()
	if _, err := f.svc.PutProjectTarget(ctx, "project-1", "api.acme.dev", put([]apigen.PutTargetDocument{textDocument("notes", "Notes.")}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.store.DeleteProject(ctx, "project-1"); err != nil {
		t.Fatal(err)
	}
	if entries, err := f.svc.store.ListTargetEntries(ctx, "project-1"); err != nil || len(entries) != 0 {
		t.Fatalf("entries = %+v, %v", entries, err)
	}
	if _, err := f.svc.store.GetTargetDocumentText(ctx, "project-1", "api.acme.dev", "notes"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("text = %v, want not found", err)
	}
}

func TestARedirectIsNotFollowed(t *testing.T) {
	f := newFixture(t)
	ctx := person()
	doc := apigen.PutTargetDocument{Name: "rest", Kind: apigen.PutTargetDocumentKindOpenapi, URL: apigen.NewOptString(f.url + "/moved.json"), SHA256: apigen.NewOptString(digest([]byte(restText)))}
	if _, err := f.svc.PutProjectTarget(ctx, "project-1", "api.acme.dev", put([]apigen.PutTargetDocument{doc}, nil, nil)); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.GetProjectTargetDocument(ctx, "project-1", "api.acme.dev", "rest")
	if statusOf(err) != http.StatusBadGateway || !strings.Contains(err.Error(), "302") {
		t.Fatalf("document = %v, want a 502 for the redirect", err)
	}
	if f.fetches.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

func TestAnInternalAddressIsNeverFetched(t *testing.T) {
	f := newFixture(t)
	ctx := person()
	sum := digest([]byte(restText))
	publicnet.Internal = func(ip netip.Addr) bool { return ip.IsLoopback() }

	// Refused when it is written...
	internal := apigen.PutTargetDocument{Name: "rest", Kind: apigen.PutTargetDocumentKindOpenapi, URL: apigen.NewOptString("https://127.0.0.1:8443/admin"), SHA256: apigen.NewOptString(sum)}
	_, err := f.svc.PutProjectTarget(ctx, "project-1", "api.acme.dev", put([]apigen.PutTargetDocument{internal}, nil, nil))
	if statusOf(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "internal address") {
		t.Fatalf("put = %v, want a 400 refusing an internal address", err)
	}

	// ...and when it is fetched: the built-in's origin is on loopback.
	_, err = f.svc.GetProjectTargetDocument(ctx, "project-1", "example.com", "rest")
	if statusOf(err) != http.StatusBadGateway || !strings.Contains(err.Error(), "internal address") {
		t.Fatalf("document = %v, want a 502 refusing an internal address", err)
	}
	if f.fetches.Load() != 0 {
		t.Fatal("an internal address was fetched")
	}
}
