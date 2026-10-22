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
)

// fakeWorkspace is a discobox's filesystem as a map, and its changes as the
// test sets them.
type fakeWorkspace struct {
	mu        sync.Mutex
	files     map[string][]byte
	revisions map[string][]byte // rev:path
	changes   []Changed
	report    func([]ahptypes.ResourceChange)
	stopped   bool
}

func (f *fakeWorkspace) ReadFile(path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

func (f *fakeWorkspace) WriteFile(path string, write FileWrite) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if write.IfMatch != "" && write.IfMatch != "v1" {
		return ErrChanged
	}
	if _, ok := f.files[path]; ok && write.CreateOnly {
		return fs.ErrExist
	}
	f.files[path] = write.Data
	return nil
}

func (f *fakeWorkspace) ReadDir(string) ([]DirEntry, error) {
	return []DirEntry{{Name: "src", Dir: true}, {Name: "go.mod"}}, nil
}

func (f *fakeWorkspace) Stat(path string, _ bool) (FileInfo, error) {
	if _, err := f.ReadFile(path); err != nil {
		return FileInfo{}, err
	}
	return FileInfo{Path: path, Type: ahptypes.ResourceTypeFile, Size: 3, ModTime: time.Unix(0, 0), Etag: "v1"}, nil
}

func (f *fakeWorkspace) Mkdir(string) error        { return nil }
func (f *fakeWorkspace) Remove(string, bool) error { return nil }

func (f *fakeWorkspace) Copy(_, _ string, replace bool) error {
	if !replace {
		return fs.ErrExist
	}
	return nil
}

func (f *fakeWorkspace) Move(string, string, bool) error { return fs.ErrInvalid }

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

func newFakeWorkspace() *fakeWorkspace {
	return &fakeWorkspace{files: map[string][]byte{"/work/go.mod": []byte("mod")}, revisions: map[string][]byte{}}
}

// startLocalHost is a host in the discobox it serves, whose working tree is
// /work.
func startLocalHost(t *testing.T, ws *fakeWorkspace, diffBase string) (*httptest.Server, context.Context, *fakeBoxes) {
	t.Helper()
	box := runningBox()
	box.Source, box.DiffBase = "/work", diffBase
	boxes := &fakeBoxes{boxes: []Box{box}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	host := New(Config{Boxes: boxes, Token: testToken, Version: "test", Poll: 20 * time.Millisecond, Workspace: ws, Logf: t.Logf})
	go func() { _ = host.Run(ctx) }()
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	return srv, ctx, boxes
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

func TestALocalHostServesItsWorkspacesFiles(t *testing.T) {
	ws := newFakeWorkspace()
	srv, ctx, _ := startLocalHost(t, ws, "")
	client, _ := connect(ctx, t, srv)
	session := "ahp-session:/sbx_1"

	data := base64.StdEncoding.EncodeToString([]byte("hello\n"))
	if err := client.Request(ctx, "resourceWrite", ahptypes.ResourceWriteParams{Channel: session, Uri: "file:///work/f.txt", Data: data, Encoding: ahptypes.ContentEncodingBase64}, nil); err != nil {
		t.Fatal(err)
	}
	utf8 := ahptypes.ContentEncodingUtf8
	var read ahptypes.ResourceReadResult
	if err := client.Request(ctx, "resourceRead", ahptypes.ResourceReadParams{Channel: session, Uri: "file:///work/f.txt", Encoding: &utf8}, &read); err != nil {
		t.Fatal(err)
	}
	if read.Data != "hello\n" || read.Encoding != ahptypes.ContentEncodingUtf8 || *read.ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("read %q (%s, %s)", read.Data, read.Encoding, *read.ContentType)
	}
	var list ahptypes.ResourceListResult
	if err := client.Request(ctx, "resourceList", ahptypes.ResourceListParams{Channel: session, Uri: "file:///work"}, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Entries) != 2 || list.Entries[0].Type != "directory" || list.Entries[1].Type != "file" {
		t.Fatalf("list = %+v", list.Entries)
	}
	var resolved ahptypes.ResourceResolveResult
	if err := client.Request(ctx, "resourceResolve", ahptypes.ResourceResolveParams{Channel: session, Uri: "file:///work/go.mod"}, &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved.Type != ahptypes.ResourceTypeFile || *resolved.Size != 3 || *resolved.Etag != "v1" {
		t.Fatalf("resolve = %+v", resolved)
	}

	// The workspace's errors are the protocol's.
	stale, yes := "v0", true
	for name, tc := range map[string]struct {
		method string
		params any
		want   int32
	}{
		"a missing file":         {"resourceResolve", ahptypes.ResourceResolveParams{Channel: session, Uri: "file:///work/nope"}, ahptypes.ErrorCodeNotFound},
		"another machine's file": {"resourceRead", ahptypes.ResourceReadParams{Channel: session, Uri: "file://elsewhere/work/go.mod"}, ahptypes.ErrorCodeNotFound},
		"create-only over one":   {"resourceWrite", ahptypes.ResourceWriteParams{Channel: session, Uri: "file:///work/go.mod", Data: "x", Encoding: utf8, CreateOnly: &yes}, ahptypes.ErrorCodeAlreadyExists},
		"a stale version":        {"resourceWrite", ahptypes.ResourceWriteParams{Channel: session, Uri: "file:///work/go.mod", Data: "x", Encoding: utf8, IfMatch: &stale}, ahptypes.ErrorCodeInvalidRequest},
		"a copy not to replace":  {"resourceCopy", ahptypes.ResourceCopyParams{Channel: session, Source: "file:///work/a", Destination: "file:///work/b", FailIfExists: &yes}, ahptypes.ErrorCodeAlreadyExists},
		"a move into itself":     {"resourceMove", ahptypes.ResourceMoveParams{Channel: session, Source: "file:///work/a", Destination: "file:///work/a/b"}, ahptypes.ErrorCodeInvalidParams},
	} {
		if err := client.Request(ctx, tc.method, tc.params, nil); rpcCode(err) != tc.want {
			t.Errorf("%s: %v, want %d", name, err, tc.want)
		}
	}
}

// The CLI's host has no discobox's files to serve.
func TestTheCLIHostServesNoFiles(t *testing.T) {
	srv, ctx := startHost(t, &fakeBoxes{boxes: []Box{runningBox()}})
	client, _ := connect(ctx, t, srv)
	var read ahptypes.ResourceReadResult
	err := client.Request(ctx, "resourceRead", ahptypes.ResourceReadParams{Channel: "ahp-session:/sbx_1", Uri: "file:///etc/hostname"}, &read)
	if rpcCode(err) != ahptypes.ErrorCodeMethodNotFound {
		t.Fatalf("resourceRead from the CLI host: %v, want MethodNotFound", err)
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
