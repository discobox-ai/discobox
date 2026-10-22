package ahphost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"

	"github.com/discobox-ai/discobox/pkg/platform"
)

// fakeWorkspace is a discobox's watches and git, as the test sets them.
type fakeWorkspace struct {
	mu        sync.Mutex
	revisions map[string][]byte // rev:path
	changes   []Changed
	report    func([]ahptypes.ResourceChange)
	stopped   bool
}

func (f *fakeWorkspace) Watch(_ string, _ bool, _, _ []string, report func([]ahptypes.ResourceChange)) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.report = report
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.stopped = true
	}, nil
}

func (f *fakeWorkspace) Changes(context.Context, string, string) ([]Changed, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changes, nil
}

func (f *fakeWorkspace) ReadRevision(_ context.Context, path, rev string) ([]byte, error) {
	data, ok := f.revisions[rev+":"+path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

// startLocalHost is a host in the discobox it serves, whose working tree is
// /work.
func startLocalHost(t *testing.T, ws *fakeWorkspace, diffBase string) (*httptest.Server, context.Context, *fakeBoxes) {
	t.Helper()
	box := runningBox()
	box.Source, box.DiffBase = "/work", diffBase
	boxes := &fakeBoxes{boxes: []Box{box}}
	boxes.setFile("sbx_1", "/work", nil)
	boxes.setFile("sbx_1", "/work/go.mod", []byte("mod"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	host := New(Config{Boxes: boxes, Token: testToken, Version: "test", Poll: 20 * time.Millisecond, Workspace: ws, Logf: t.Logf})
	go func() { _ = host.Run(ctx) }()
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	return srv, ctx, boxes
}

func newFakeWorkspace() *fakeWorkspace {
	return &fakeWorkspace{revisions: map[string][]byte{}}
}

// subscribeRaw subscribes and decodes the snapshot's state into state: the
// SDK's client tells a snapshot's kind by its fields, which a changeset's and
// a watch's do not say.
func subscribeRaw(ctx context.Context, t *testing.T, client *ahp.Client, uri string, state any) *ahp.Subscription {
	t.Helper()
	sub := client.AttachSubscription(uri)
	var res struct {
		Snapshot struct {
			State json.RawMessage `json:"state"`
		} `json:"snapshot"`
	}
	if err := client.Request(ctx, "subscribe", ahptypes.SubscribeParams{Channel: uri}, &res); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(res.Snapshot.State, state); err != nil {
		t.Fatal(err)
	}
	return sub
}

func rpcCode(err error) int32 {
	var rerr *ahp.RPCError
	if errors.As(err, &rerr) {
		return rerr.Code
	}
	return 0
}

func TestALocalSessionsFolderIsALocalPath(t *testing.T) {
	srv, ctx, boxes := startLocalHost(t, newFakeWorkspace(), "")
	client, _ := connect(ctx, t, srv)
	res, _, err := client.Subscribe(ctx, "ahp-session:/sbx_1")
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Snapshot.State.Session.WorkingDirectories; len(got) != 1 || got[0] != "file:///work" {
		t.Fatalf("working directories = %v, want [file:///work]", got)
	}
	// A terminal in any folder of the discobox is a shell in it.
	cwd := "file:///work/sub"
	if err := client.Request(ctx, "createTerminal", ahptypes.CreateTerminalParams{Channel: "agenthost-terminal:/t1", Cwd: &cwd,
		Claim: ahptypes.TerminalClaim{Value: &ahptypes.TerminalClientClaim{Kind: ahptypes.TerminalClaimKindClient, ClientId: "client-1"}}}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a shell", func() bool { _, _, _, _, shells := boxes.snapshot(); return len(shells) == 1 })
}

// A host in a discobox makes no discoboxes: a new session is refused before
// its first message — the prompt — could be lost.
func TestALocalHostRefusesNewSessions(t *testing.T) {
	srv, ctx, _ := startLocalHost(t, newFakeWorkspace(), "")
	client, _ := connect(ctx, t, srv)
	provider := "claude"
	err := client.Request(ctx, "createSession", ahptypes.CreateSessionParams{Channel: "ahp-session:/new", Provider: &provider}, nil)
	if rpcCode(err) != ahptypes.ErrorCodeInvalidRequest {
		t.Fatalf("createSession: %v, want InvalidRequest", err)
	}
}

// exerciseFiles drives the resource* commands at a discobox's /work through
// uri, which names files the way the host does.
func exerciseFiles(ctx context.Context, t *testing.T, client *ahp.Client, boxes *fakeBoxes, uri func(string) string) {
	t.Helper()
	session := "ahp-session:/sbx_1"
	data := base64.StdEncoding.EncodeToString([]byte("hello\n"))
	if err := client.Request(ctx, "resourceWrite", ahptypes.ResourceWriteParams{Channel: session, Uri: uri("/work/f.txt"), Data: data, Encoding: ahptypes.ContentEncodingBase64}, nil); err != nil {
		t.Fatal(err)
	}
	// An append keeps what is there: it is spliced and written whole.
	appendMode := ahptypes.ResourceWriteModeAppend
	if err := client.Request(ctx, "resourceWrite", ahptypes.ResourceWriteParams{Channel: session, Uri: uri("/work/f.txt"), Data: "world\n", Encoding: ahptypes.ContentEncodingUtf8, Mode: &appendMode}, nil); err != nil {
		t.Fatal(err)
	}
	utf8 := ahptypes.ContentEncodingUtf8
	var read ahptypes.ResourceReadResult
	if err := client.Request(ctx, "resourceRead", ahptypes.ResourceReadParams{Channel: session, Uri: uri("/work/f.txt"), Encoding: &utf8}, &read); err != nil {
		t.Fatal(err)
	}
	if read.Data != "hello\nworld\n" || read.Encoding != ahptypes.ContentEncodingUtf8 || *read.ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("read %q (%s, %s)", read.Data, read.Encoding, *read.ContentType)
	}
	if err := client.Request(ctx, "resourceMkdir", ahptypes.ResourceMkdirParams{Channel: session, Uri: uri("/work/src")}, nil); err != nil {
		t.Fatal(err)
	}
	// A link lists as what it points at: a linked folder is a folder.
	boxes.setLink("sbx_1", "/work/lib", "src")
	var list ahptypes.ResourceListResult
	if err := client.Request(ctx, "resourceList", ahptypes.ResourceListParams{Channel: session, Uri: uri("/work")}, &list); err != nil {
		t.Fatal(err)
	}
	want := []ahptypes.DirectoryEntry{{Name: "f.txt", Type: "file"}, {Name: "go.mod", Type: "file"}, {Name: "lib", Type: "directory"}, {Name: "src", Type: "directory"}}
	if len(list.Entries) != len(want) {
		t.Fatalf("list = %+v", list.Entries)
	}
	for i := range want {
		if list.Entries[i] != want[i] {
			t.Fatalf("list = %+v, want %+v", list.Entries, want)
		}
	}
	var resolved ahptypes.ResourceResolveResult
	if err := client.Request(ctx, "resourceResolve", ahptypes.ResourceResolveParams{Channel: session, Uri: uri("/work/go.mod")}, &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved.Type != ahptypes.ResourceTypeFile || *resolved.Size != 3 || *resolved.Etag != "3:mod" || resolved.Uri != uri("/work/go.mod") {
		t.Fatalf("resolve = %+v", resolved)
	}
	if err := client.Request(ctx, "resourceMove", ahptypes.ResourceMoveParams{Channel: session, Source: uri("/work/f.txt"), Destination: uri("/work/g.txt")}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := boxes.StatFile(ctx, "sbx_1", "/work/g.txt"); err != nil {
		t.Fatalf("the move did not land: %v", err)
	}

	// The discobox's errors are the protocol's.
	stale, yes := "0:", true
	for name, tc := range map[string]struct {
		method string
		params any
		want   int32
	}{
		"a missing file":        {"resourceResolve", ahptypes.ResourceResolveParams{Channel: session, Uri: uri("/work/nope")}, ahptypes.ErrorCodeNotFound},
		"create-only over one":  {"resourceWrite", ahptypes.ResourceWriteParams{Channel: session, Uri: uri("/work/go.mod"), Data: "x", Encoding: utf8, CreateOnly: &yes}, ahptypes.ErrorCodeAlreadyExists},
		"a stale version":       {"resourceWrite", ahptypes.ResourceWriteParams{Channel: session, Uri: uri("/work/go.mod"), Data: "x", Encoding: utf8, IfMatch: &stale}, ahptypes.ErrorCodeInvalidRequest},
		"an append, stale":      {"resourceWrite", ahptypes.ResourceWriteParams{Channel: session, Uri: uri("/work/go.mod"), Data: "x", Encoding: utf8, IfMatch: &stale, Mode: &appendMode}, ahptypes.ErrorCodeInvalidRequest},
		"a copy not to replace": {"resourceCopy", ahptypes.ResourceCopyParams{Channel: session, Source: uri("/work/g.txt"), Destination: uri("/work/go.mod"), FailIfExists: &yes}, ahptypes.ErrorCodeAlreadyExists},
		"a copy into itself":    {"resourceCopy", ahptypes.ResourceCopyParams{Channel: session, Source: uri("/work/src"), Destination: uri("/work/src/b")}, ahptypes.ErrorCodeInvalidParams},
	} {
		if err := client.Request(ctx, tc.method, tc.params, nil); rpcCode(err) != tc.want {
			t.Errorf("%s: %v, want %d", name, err, tc.want)
		}
	}
	if data, _ := boxes.ReadFile(ctx, "sbx_1", "/work/go.mod"); string(data) != "mod" {
		t.Fatalf("a refused write changed the file: %q", data)
	}
}

func TestALocalHostServesItsDiscoboxsFiles(t *testing.T) {
	srv, ctx, boxes := startLocalHost(t, newFakeWorkspace(), "")
	client, _ := connect(ctx, t, srv)
	exerciseFiles(ctx, t, client, boxes, func(p string) string { return "file://" + p })
	var read ahptypes.ResourceReadResult
	err := client.Request(ctx, "resourceRead", ahptypes.ResourceReadParams{Channel: "ahp-session:/sbx_1", Uri: "file://elsewhere/work/go.mod"}, &read)
	if rpcCode(err) != ahptypes.ErrorCodeNotFound {
		t.Fatalf("reading a file that names another machine: %v, want NotFound", err)
	}
}

// The CLI's host serves every discobox's files, each named by the URI's
// authority as its session's folder is (ADR 26-10-22-499 §4).
func TestTheCLIHostServesEachDiscoboxsFiles(t *testing.T) {
	boxes := &fakeBoxes{boxes: []Box{runningBox()}}
	boxes.setFile("sbx_1", "/work", nil)
	boxes.setFile("sbx_1", "/work/go.mod", []byte("mod"))
	srv, ctx := startHost(t, boxes)
	client, _ := connect(ctx, t, srv)
	exerciseFiles(ctx, t, client, boxes, func(p string) string { return "file://sbx_1" + p })
	// The folder a session advertises is the discobox's working tree, not
	// the client directory it was cut from: listing it lists the discobox.
	res, _, err := client.Subscribe(ctx, "ahp-session:/sbx_1")
	if err != nil {
		t.Fatal(err)
	}
	folder := res.Snapshot.State.Session.WorkingDirectories[0]
	var list ahptypes.ResourceListResult
	if err := client.Request(ctx, "resourceList", ahptypes.ResourceListParams{Channel: "ahp-session:/sbx_1", Uri: folder}, &list); err != nil || len(list.Entries) == 0 {
		t.Fatalf("listing the session's folder %s: %+v, %v", folder, list.Entries, err)
	}
	var read ahptypes.ResourceReadResult
	err = client.Request(ctx, "resourceRead", ahptypes.ResourceReadParams{Channel: "ahp-session:/sbx_1", Uri: "file:///work/go.mod"}, &read)
	if rpcCode(err) != ahptypes.ErrorCodeNotFound {
		t.Fatalf("a file URI naming no discobox: %v, want NotFound", err)
	}
}

// A Windows discobox's folder has a drive: its URI is `/C:/…`, and the
// discobox is asked for `C:\…`, by its platform's own rules — a drive's root
// too, which VS Code spells `/c%3A/`.
func TestAWindowsDiscoboxsFilesAreReachedByTheirDrive(t *testing.T) {
	box := runningBox()
	box.Workdir, box.Platform = `C:\workspace`, platform.Platform{OS: "windows", Arch: "amd64"}
	boxes := &fakeBoxes{boxes: []Box{box}}
	boxes.setFile("sbx_1", `C:\workspace`, nil)
	boxes.setFile("sbx_1", `C:\workspace\go.mod`, []byte("mod"))
	boxes.setFile("sbx_1", `c:\`, nil)
	boxes.setFile("sbx_1", `c:\workspace`, nil)
	srv, ctx := startHost(t, boxes)
	client, _ := connect(ctx, t, srv)
	res, _, err := client.Subscribe(ctx, "ahp-session:/sbx_1")
	if err != nil {
		t.Fatal(err)
	}
	folder := res.Snapshot.State.Session.WorkingDirectories[0]
	if folder != "file://sbx_1/C:/workspace" {
		t.Fatalf("folder = %s, want file://sbx_1/C:/workspace", folder)
	}
	var list ahptypes.ResourceListResult
	if err := client.Request(ctx, "resourceList", ahptypes.ResourceListParams{Channel: "ahp-session:/sbx_1", Uri: folder}, &list); err != nil || len(list.Entries) != 1 || list.Entries[0].Name != "go.mod" {
		t.Fatalf("listing %s: %+v, %v", folder, list.Entries, err)
	}
	var resolved ahptypes.ResourceResolveResult
	if err := client.Request(ctx, "resourceResolve", ahptypes.ResourceResolveParams{Channel: "ahp-session:/sbx_1", Uri: folder + "/go.mod"}, &resolved); err != nil || resolved.Uri != folder+"/go.mod" {
		t.Fatalf("resolving %s/go.mod: %+v, %v", folder, resolved, err)
	}
	if err := client.Request(ctx, "resourceList", ahptypes.ResourceListParams{Channel: "ahp-session:/sbx_1", Uri: "file://sbx_1/c%3A/"}, &list); err != nil || len(list.Entries) != 1 || list.Entries[0].Name != "workspace" {
		t.Fatalf("listing the drive's root: %+v, %v", list.Entries, err)
	}
}

// On a Linux discobox a path that looks like a drive is a path like any
// other: `/C:/work` is a directory named `C:`.
func TestALinuxPathThatLooksLikeADriveIsAPath(t *testing.T) {
	boxes := &fakeBoxes{boxes: []Box{runningBox()}}
	boxes.setFile("sbx_1", "/C:/work", []byte("x"))
	srv, ctx := startHost(t, boxes)
	client, _ := connect(ctx, t, srv)
	var resolved ahptypes.ResourceResolveResult
	if err := client.Request(ctx, "resourceResolve", ahptypes.ResourceResolveParams{Channel: "ahp-session:/sbx_1", Uri: "file://sbx_1/C:/work"}, &resolved); err != nil || *resolved.Size != 1 {
		t.Fatalf("resolving /C:/work on Linux: %+v, %v", resolved, err)
	}
}

// Looking at a stopped discobox's files does not start it.
func TestAStoppedDiscoboxsFilesAreNotWoken(t *testing.T) {
	box := runningBox()
	box.State = BoxStopped
	boxes := &fakeBoxes{boxes: []Box{box}}
	boxes.setFile("sbx_1", "/work", nil)
	srv, ctx := startHost(t, boxes)
	client, _ := connect(ctx, t, srv)
	var list ahptypes.ResourceListResult
	err := client.Request(ctx, "resourceList", ahptypes.ResourceListParams{Channel: "ahp-session:/sbx_1", Uri: "file://sbx_1/work"}, &list)
	if rpcCode(err) != ahptypes.ErrorCodeInvalidRequest {
		t.Fatalf("listing a stopped discobox: %v, want InvalidRequest", err)
	}
	if _, _, verbs, _, _ := boxes.snapshot(); len(verbs) != 0 {
		t.Fatalf("the discobox was acted on: %v", verbs)
	}
}

// Only a host in the discobox can watch its files.
func TestTheCLIHostServesNoWatches(t *testing.T) {
	srv, ctx := startHost(t, &fakeBoxes{boxes: []Box{runningBox()}})
	client, _ := connect(ctx, t, srv)
	err := client.Request(ctx, "createResourceWatch", ahptypes.CreateResourceWatchParams{Channel: "ahp-session:/sbx_1", Uri: "file://sbx_1/work"}, nil)
	if rpcCode(err) != ahptypes.ErrorCodeMethodNotFound {
		t.Fatalf("createResourceWatch from the CLI host: %v, want MethodNotFound", err)
	}
}

func TestAWatchReportsUntilItIsLetGo(t *testing.T) {
	ws := newFakeWorkspace()
	srv, ctx, _ := startLocalHost(t, ws, "")
	client, _ := connect(ctx, t, srv)
	recursive := true
	var created ahptypes.CreateResourceWatchResult
	if err := client.Request(ctx, "createResourceWatch", ahptypes.CreateResourceWatchParams{Channel: "ahp-session:/sbx_1", Uri: "file:///work", Recursive: &recursive}, &created); err != nil {
		t.Fatal(err)
	}
	var state ahptypes.ResourceWatchState
	sub := subscribeRaw(ctx, t, client, created.Channel, &state)
	if !state.Recursive || state.Root != "file:///work" {
		t.Fatalf("watch state = %+v", state)
	}
	ws.mu.Lock()
	report := ws.report
	ws.mu.Unlock()
	report([]ahptypes.ResourceChange{{Uri: "file:///work/f", Type: ahptypes.ResourceChangeTypeAdded}})
	changed, ok := nextAction(t, sub).Action.Value.(*ahptypes.ResourceWatchChangedAction)
	if !ok || string(changed.Changes) != `{"items":[{"uri":"file:///work/f","type":"added"}]}` {
		t.Fatalf("action = %+v", changed)
	}
	// Requests and notifications are handled in the order they come, so the
	// subscribe below is answered after the unsubscribe let the watch go.
	if err := client.Unsubscribe(ctx, created.Channel); err != nil {
		t.Fatal(err)
	}
	err := client.Request(ctx, "subscribe", ahptypes.SubscribeParams{Channel: created.Channel}, nil)
	if rpcCode(err) != ahptypes.ErrorCodeNotFound {
		t.Fatalf("a watch nobody subscribes to is still there: %v", err)
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if !ws.stopped {
		t.Fatal("the workspace's watch was not stopped")
	}
}

func TestChangesetsAreTheWorkspacesChanges(t *testing.T) {
	ws := newFakeWorkspace()
	one, two := int64(1), int64(2)
	ws.changes = []Changed{
		{Path: "/work/edited.txt", Before: true, After: true, Added: &two, Removed: &one},
		{Path: "/work/new.txt", After: true, Added: &one},
		{Path: "/work/gone.txt", Before: true},
	}
	ws.revisions["base1:/work/edited.txt"] = []byte("old\n")
	srv, ctx, _ := startLocalHost(t, ws, "base1")
	client, _ := connect(ctx, t, srv)
	res, _, err := client.Subscribe(ctx, "ahp-session:/sbx_1")
	if err != nil {
		t.Fatal(err)
	}
	catalog := res.Snapshot.State.Session.Changesets
	if len(catalog) != 2 || catalog[0].ChangeKind != "branch" || catalog[1].ChangeKind != "uncommitted" {
		t.Fatalf("changesets = %+v", catalog)
	}
	var state ahptypes.ChangesetState
	sub := subscribeRaw(ctx, t, client, catalog[0].UriTemplate, &state)
	for state.Status != ahptypes.ChangesetStatusReady {
		ahp.ApplyActionToChangeset(&state, nextAction(t, sub).Action)
	}
	if len(state.Files) != 3 {
		t.Fatalf("files = %+v", state.Files)
	}
	edited, added, removed := state.Files[0].Edit, state.Files[1].Edit, state.Files[2].Edit
	if edited.Before == nil || edited.After == nil || *edited.Diff.Added != 2 || added.Before != nil || removed.After != nil {
		t.Fatalf("edits = %+v %+v %+v", edited, added, removed)
	}
	// The before side reads the file as the base has it.
	utf8 := ahptypes.ContentEncodingUtf8
	var read ahptypes.ResourceReadResult
	if err := client.Request(ctx, "resourceRead", ahptypes.ResourceReadParams{Channel: "ahp-session:/sbx_1", Uri: edited.Before.Content.Uri, Encoding: &utf8}, &read); err != nil {
		t.Fatal(err)
	}
	if read.Data != "old\n" {
		t.Fatalf("before = %q", read.Data)
	}
}

// The CLI's host offers no changesets: it has no working tree to read.
func TestTheCLIHostOffersNoChangesets(t *testing.T) {
	box := runningBox()
	box.DiffBase = "base1"
	srv, ctx := startHost(t, &fakeBoxes{boxes: []Box{box}})
	client, _ := connect(ctx, t, srv)
	res, _, err := client.Subscribe(ctx, "ahp-session:/sbx_1")
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Snapshot.State.Session.Changesets; len(got) != 0 {
		t.Fatalf("changesets = %+v", got)
	}
}
