package ahphost

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/microsoft/agent-host-protocol/clients/go/ahpws"

	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/discobox/pkg/programstatus"
)

// fakeBoxes is Discobox as the host sees it, recording what it was asked.
type fakeBoxes struct {
	mu      sync.Mutex
	boxes   []tui.Sandbox
	execs   map[string][]tui.Exec
	opened  []string // sandbox/exec attached
	inputs  []string // sandbox/exec: text
	verbs   []string // verb sandbox
	runs    []tui.RunRequest
	shells  []string
	streams []*fakeStream
	// listingCut is a listing whose context ended as soon as List returned.
	listingCut bool
}

func (f *fakeBoxes) List(ctx context.Context) (tui.Listing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The console's listing leaves a slow server's request running past the
	// call; whether the context it was given is still alive a moment later is
	// whether that request would be.
	go func() {
		time.Sleep(30 * time.Millisecond)
		if ctx.Err() != nil {
			f.mu.Lock()
			f.listingCut = true
			f.mu.Unlock()
		}
	}()
	return tui.Listing{Sandboxes: append([]tui.Sandbox(nil), f.boxes...)}, nil
}

func (f *fakeBoxes) Harnesses(context.Context, string) ([]tui.Harness, error) {
	return []tui.Harness{{Slug: "claude", Name: "Claude Code", State: tui.HarnessEnabled}}, nil
}

func (f *fakeBoxes) Run(_ context.Context, req tui.RunRequest, _ func(string), _ func()) (tui.Sandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, req)
	box := tui.Sandbox{ID: "sbx_new", Name: "new", State: tui.StateStarting, Harness: req.Harness, Created: time.Now()}
	f.boxes = append(f.boxes, box)
	return box, nil
}

func (f *fakeBoxes) Do(_ context.Context, verb tui.Verb, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verbs = append(f.verbs, string(verb)+" "+id)
	return nil
}

func (f *fakeBoxes) Rename(context.Context, string, string) error { return nil }

func (f *fakeBoxes) Execs(_ context.Context, id string) ([]tui.Exec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.execs[id], nil
}

func (f *fakeBoxes) OpenExec(ctx context.Context, id, execID string, _, _ int) (tui.Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, id+"/"+execID)
	s := attached(ctx)
	f.streams = append(f.streams, s)
	return s, nil
}

func (f *fakeBoxes) NewShell(ctx context.Context, id string, _, _ int) (tui.Exec, tui.Terminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shells = append(f.shells, id)
	s := attached(ctx)
	f.streams = append(f.streams, s)
	return tui.Exec{ID: "shell-1", Tty: true, Live: true}, s, nil
}

func (f *fakeBoxes) NewTerminal(ctx context.Context, _ string, _, _ int) (tui.Exec, tui.Terminal, error) {
	return tui.Exec{ID: "term-2", Harness: "claude", Live: true}, attached(ctx), nil
}

func (f *fakeBoxes) EndExec(context.Context, string, string) error { return nil }

func (f *fakeBoxes) SendInput(_ context.Context, id, execID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, id+"/"+execID+": "+text)
	return nil
}

func (f *fakeBoxes) PutImage(context.Context, string, string, []byte) (string, error) {
	return "/tmp/img.png", nil
}

func (f *fakeBoxes) snapshot() (opened, inputs, verbs []string, runs []tui.RunRequest, shells []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.opened...), append([]string(nil), f.inputs...), append([]string(nil), f.verbs...),
		append([]tui.RunRequest(nil), f.runs...), append([]string(nil), f.shells...)
}

// fakeStream is an attach: what is sent on out is read, what is written is
// kept.
type fakeStream struct {
	out     chan []byte
	mu      sync.Mutex
	written []byte
	closed  chan struct{}
	once    sync.Once
}

func newFakeStream() *fakeStream {
	return &fakeStream{out: make(chan []byte, 16), closed: make(chan struct{})}
}

// attached is a stream opened under ctx, which ends with it, as a real
// attach does: one opened under a request's context dies with the request.
func attached(ctx context.Context) *fakeStream {
	s := newFakeStream()
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-s.closed:
		}
	}()
	return s
}

func (s *fakeStream) Read(p []byte) (int, error) {
	select {
	case b := <-s.out:
		return copy(p, b), nil
	case <-s.closed:
		return 0, io.EOF
	}
}

func (s *fakeStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, p...)
	return len(p), nil
}

func (s *fakeStream) typed() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.written)
}

func (s *fakeStream) Close() error                     { s.once.Do(func() { close(s.closed) }); return nil }
func (s *fakeStream) Resize(int, int) error            { return nil }
func (s *fakeStream) Repaint() error                   { return nil }
func (s *fakeStream) Events() <-chan tui.TerminalEvent { return nil }

const testToken = "secret"

func runningBox() tui.Sandbox {
	return tui.Sandbox{
		ID: "sbx_1", Name: "fix-tests", State: tui.StateRunning, Harness: "claude", HarnessName: "Claude Code",
		Source: "/home/me/src/app", Created: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Agent: programstatus.Status{State: programstatus.Working, Msg: "running the tests", ReportedAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)},
	}
}

func startHost(t *testing.T, boxes *fakeBoxes) (*httptest.Server, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	host := New(Config{Boxes: boxes, Token: testToken, Version: "test", Poll: 20 * time.Millisecond, Logf: t.Logf})
	go func() { _ = host.Run(ctx) }()
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	return srv, ctx
}

func connect(ctx context.Context, t *testing.T, srv *httptest.Server) (*ahp.Client, *ahptypes.InitializeResult) {
	t.Helper()
	transport, err := ahpws.Connect(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/?tkn="+testToken)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ahp.Connect(ctx, transport, ahp.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	init, err := client.Initialize(ctx, "client-1", []string{"1.0.0", "0.9.0"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client, init
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// subscribeTerminal subscribes to a terminal. The SDK's client tells a
// snapshot's kind by its fields, and takes a terminal's — which has a
// lifecycle — for a session's, so this one is read by hand.
func subscribeTerminal(ctx context.Context, t *testing.T, client *ahp.Client, uri string) (*ahptypes.TerminalState, *ahp.Subscription) {
	t.Helper()
	sub := client.AttachSubscription(uri)
	var res struct {
		Snapshot struct {
			State ahptypes.TerminalState `json:"state"`
		} `json:"snapshot"`
	}
	if err := client.Request(ctx, "subscribe", ahptypes.SubscribeParams{Channel: uri}, &res); err != nil {
		t.Fatal(err)
	}
	return &res.Snapshot.State, sub
}

// screenCall is the running turn's terminal tool call, once it has its
// terminal.
func screenCall(state *ahptypes.ChatState) (*ahptypes.ToolCallRunningState, bool) {
	if state.ActiveTurn == nil || len(state.ActiveTurn.ResponseParts) == 0 {
		return nil, false
	}
	part, ok := state.ActiveTurn.ResponseParts[0].Value.(*ahptypes.ToolCallResponsePart)
	if !ok {
		return nil, false
	}
	running, ok := part.ToolCall.Value.(*ahptypes.ToolCallRunningState)
	if !ok || len(running.Content) == 0 {
		return nil, false
	}
	return running, true
}

// nextAction is the next action envelope on a subscription.
func nextAction(t *testing.T, sub *ahp.Subscription) ahptypes.ActionEnvelope {
	t.Helper()
	for {
		select {
		case ev := <-sub.Events():
			if a, ok := ev.(ahp.SubscriptionEventAction); ok {
				return a.Envelope
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for an action")
		}
	}
}

func TestAConnectionNeedsTheToken(t *testing.T) {
	srv, _ := startHost(t, &fakeBoxes{})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/?tkn=wrong", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", res.StatusCode)
	}
}

func TestADiscoboxIsASession(t *testing.T) {
	boxes := &fakeBoxes{boxes: []tui.Sandbox{runningBox()}}
	srv, ctx := startHost(t, boxes)
	client, init := connect(ctx, t, srv)

	if init.ProtocolVersion != "1.0.0" {
		t.Fatalf("protocol %q", init.ProtocolVersion)
	}
	if len(init.Snapshots) == 0 || init.Snapshots[0].State.Root == nil {
		t.Fatalf("no root snapshot: %+v", init.Snapshots)
	}
	agents := init.Snapshots[0].State.Root.Agents
	if len(agents) != 1 || agents[0].Provider != "claude" || len(agents[0].Models) == 0 {
		t.Fatalf("agents %+v", agents)
	}

	var list ahptypes.ListSessionsResult
	if err := client.Request(ctx, "listSessions", ahptypes.ListSessionsParams{Channel: ahptypes.RootResourceURI}, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("sessions %+v", list.Items)
	}
	sum := list.Items[0]
	if sum.Resource != "ahp-session:/sbx_1" || sum.Provider != "claude" || sum.Title != "fix-tests" {
		t.Fatalf("summary %+v", sum)
	}
	if sum.Status != ahptypes.SessionStatusInProgress {
		t.Fatalf("status %d, want in progress for a working agent", sum.Status)
	}
	if sum.Activity == nil || *sum.Activity != "running the tests" {
		t.Fatalf("activity %v", sum.Activity)
	}
	if sum.DefaultChat == nil || *sum.DefaultChat != "ahp-chat:/sbx_1/primary" {
		t.Fatalf("default chat %v", sum.DefaultChat)
	}
	if len(sum.WorkingDirectories) != 1 || sum.WorkingDirectories[0] != "file://sbx_1/home/me/src/app" {
		t.Fatalf("working directories %v", sum.WorkingDirectories)
	}
}

func TestAStatusIsWhatTheAgentReported(t *testing.T) {
	for _, tc := range []struct {
		box  func(*tui.Sandbox)
		want ahptypes.SessionStatus
	}{
		{func(b *tui.Sandbox) { b.Agent.State = programstatus.Idle }, ahptypes.SessionStatusIdle},
		{func(b *tui.Sandbox) { b.Agent.State = programstatus.Done }, ahptypes.SessionStatusIdle},
		{func(b *tui.Sandbox) { b.Agent.State = programstatus.Blocked }, ahptypes.SessionStatusInputNeeded},
		{func(b *tui.Sandbox) { b.Agent.State = programstatus.Error }, ahptypes.SessionStatusError},
		{func(b *tui.Sandbox) { b.Agent = programstatus.Status{} }, ahptypes.SessionStatusIdle},
		{func(b *tui.Sandbox) { b.State = tui.StateStopped }, ahptypes.SessionStatusIdle},
		{func(b *tui.Sandbox) { b.State = tui.StateArchived }, ahptypes.SessionStatusIdle | ahptypes.SessionStatusIsArchived},
	} {
		box := runningBox()
		tc.box(&box)
		h := New(Config{})
		s := h.newSessionLocked(sessionScheme+box.ID, &box)
		if got := h.statusLocked(s); got != tc.want {
			t.Errorf("%+v: status %d, want %d", box.Agent, got, tc.want)
		}
	}
}

func TestAChatShowsItsTerminalLive(t *testing.T) {
	boxes := &fakeBoxes{
		boxes: []tui.Sandbox{runningBox()},
		execs: map[string][]tui.Exec{"sbx_1": {{ID: "exec-primary", Harness: "claude", Primary: true, Live: true}}},
	}
	srv, ctx := startHost(t, boxes)
	client, _ := connect(ctx, t, srv)

	if _, _, err := client.Subscribe(ctx, "ahp-session:/sbx_1"); err != nil {
		t.Fatal(err)
	}
	res, _, err := client.Subscribe(ctx, "ahp-chat:/sbx_1/primary")
	if err != nil {
		t.Fatal(err)
	}
	chat := res.Snapshot.State.Chat
	if chat == nil || chat.ActiveTurn == nil || len(chat.ActiveTurn.ResponseParts) != 1 {
		t.Fatalf("chat %+v", chat)
	}
	part := chat.ActiveTurn.ResponseParts[0].Value.(*ahptypes.ToolCallResponsePart)
	running, ok := part.ToolCall.Value.(*ahptypes.ToolCallRunningState)
	if !ok {
		t.Fatalf("tool call %T, want running", part.ToolCall.Value)
	}
	if running.ToolInput == nil || running.ToolInput.Inline == nil || *running.ToolInput.Inline != "discobox attach --raw sbx_1" {
		t.Fatalf("tool input %+v", running.ToolInput)
	}
	if string(running.Meta["toolKind"]) != `"terminal"` {
		t.Fatalf("meta %s", running.Meta["toolKind"])
	}
	content := running.Content[0].Value.(*ahptypes.ToolResultTerminalContent)

	// Watching the screen attaches it, by the exec's own ID: an attach by
	// ID never relaunches it.
	_, screen := subscribeTerminal(ctx, t, client, content.Resource)
	eventually(t, "the attach", func() bool {
		opened, _, _, _, _ := boxes.snapshot()
		return len(opened) == 1 && opened[0] == "sbx_1/exec-primary"
	})
	boxes.mu.Lock()
	stream := boxes.streams[0]
	boxes.mu.Unlock()

	// A character split across two reads arrives whole.
	stream.out <- []byte("caf\xc3")
	stream.out <- []byte("\xa9\r\n")
	var got string
	for got != "café\r\n" {
		env := nextAction(t, screen)
		if d, ok := env.Action.Value.(*ahptypes.TerminalDataAction); ok {
			got += d.Data
		}
	}

	if _, err := client.Dispatch(ctx, content.Resource, ahptypes.StateAction{Value: &ahptypes.TerminalInputAction{Type: ahptypes.ActionTypeTerminalInput, Data: "ls\r"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the keystrokes", func() bool { return stream.typed() == "ls\r" })
}

func TestAQueuedMessageIsTypedIntoTheTerminal(t *testing.T) {
	boxes := &fakeBoxes{boxes: []tui.Sandbox{runningBox()}}
	srv, ctx := startHost(t, boxes)
	client, _ := connect(ctx, t, srv)
	chatURI := "ahp-chat:/sbx_1/primary"
	res, sub, err := client.Subscribe(ctx, chatURI)
	if err != nil {
		t.Fatal(err)
	}
	state := *res.Snapshot.State.Chat
	message := ahptypes.Message{Text: "run the tests again", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}}
	if _, err := client.Dispatch(ctx, chatURI, ahptypes.StateAction{Value: &ahptypes.ChatPendingMessageSetAction{
		Type: ahptypes.ActionTypeChatPendingMessageSet, Kind: ahptypes.PendingMessageKindQueued, Id: "q1", Message: message,
	}}); err != nil {
		t.Fatal(err)
	}
	// Fold what comes back the way a client does, until the queued message
	// is the running turn's.
	for {
		if _, ok := screenCall(&state); ok && state.ActiveTurn.Message.Text == message.Text {
			break
		}
		env := nextAction(t, sub)
		ahp.ApplyActionToChat(&state, env.Action)
	}
	if len(state.QueuedMessages) != 0 {
		t.Fatalf("queued %+v, want it consumed", state.QueuedMessages)
	}
	if len(state.Turns) != 1 || state.Turns[0].State != ahptypes.TurnStateComplete {
		t.Fatalf("turns %+v, want the attach turn completed", state.Turns)
	}
	eventually(t, "the message", func() bool {
		_, inputs, _, _, _ := boxes.snapshot()
		return len(inputs) == 1 && inputs[0] == "sbx_1/primary: run the tests again"
	})
}

func TestObservingAStoppedDiscoboxDoesNotWakeIt(t *testing.T) {
	box := runningBox()
	box.State = tui.StateStopped
	boxes := &fakeBoxes{boxes: []tui.Sandbox{box}}
	srv, ctx := startHost(t, boxes)
	client, _ := connect(ctx, t, srv)
	if _, _, err := client.Subscribe(ctx, "ahp-session:/sbx_1"); err != nil {
		t.Fatal(err)
	}
	res, _, err := client.Subscribe(ctx, "ahp-chat:/sbx_1/primary")
	if err != nil {
		t.Fatal(err)
	}
	call, ok := screenCall(res.Snapshot.State.Chat)
	if !ok {
		t.Fatal("no screen")
	}
	screenURI := call.Content[0].Value.(*ahptypes.ToolResultTerminalContent).Resource
	subscribeTerminal(ctx, t, client, screenURI)
	time.Sleep(200 * time.Millisecond)
	opened, _, verbs, _, _ := boxes.snapshot()
	if len(opened) != 0 || len(verbs) != 0 {
		t.Fatalf("opened %v, verbs %v: watching started something", opened, verbs)
	}

	// Typing into it is asking for it.
	if _, err := client.Dispatch(ctx, screenURI, ahptypes.StateAction{Value: &ahptypes.TerminalInputAction{Type: ahptypes.ActionTypeTerminalInput, Data: "x"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a start", func() bool {
		_, _, verbs, _, _ := boxes.snapshot()
		return len(verbs) == 1 && verbs[0] == "start sbx_1"
	})
}

func TestDisposingASessionArchivesItsDiscobox(t *testing.T) {
	boxes := &fakeBoxes{boxes: []tui.Sandbox{runningBox()}}
	srv, ctx := startHost(t, boxes)
	client, _ := connect(ctx, t, srv)
	if err := client.Request(ctx, "disposeSession", ahptypes.DisposeSessionParams{Channel: "ahp-session:/sbx_1"}, nil); err != nil {
		t.Fatal(err)
	}
	_, _, verbs, _, _ := boxes.snapshot()
	if len(verbs) != 1 || verbs[0] != "archive sbx_1" {
		t.Fatalf("verbs %v, want an archive", verbs)
	}
}

func TestATerminalInASessionsDirectoryIsAShellInItsDiscobox(t *testing.T) {
	boxes := &fakeBoxes{boxes: []tui.Sandbox{runningBox()}}
	srv, ctx := startHost(t, boxes)
	client, _ := connect(ctx, t, srv)
	cwd := "file://sbx_1/home/me/src/app"
	err := client.Request(ctx, "createTerminal", ahptypes.CreateTerminalParams{
		Channel: "agenthost-terminal:/t1", Cwd: &cwd,
		Claim: ahptypes.TerminalClaim{Value: &ahptypes.TerminalClientClaim{Kind: ahptypes.TerminalClaimKindClient, ClientId: "client-1"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, _, shells := boxes.snapshot()
	if len(shells) != 1 || shells[0] != "sbx_1" {
		t.Fatalf("shells %v", shells)
	}
	res, _, err := client.Subscribe(ctx, ahptypes.RootResourceURI)
	if err != nil {
		t.Fatal(err)
	}
	if terms := res.Snapshot.State.Root.Terminals; len(terms) != 1 || terms[0].Resource != "agenthost-terminal:/t1" {
		t.Fatalf("root terminals %+v", terms)
	}

	// The shell outlives the request that made it.
	_, sub := subscribeTerminal(ctx, t, client, "agenthost-terminal:/t1")
	boxes.mu.Lock()
	stream := boxes.streams[len(boxes.streams)-1]
	boxes.mu.Unlock()
	stream.out <- []byte("$ ")
	for {
		env := nextAction(t, sub)
		if _, ok := env.Action.Value.(*ahptypes.TerminalExitedAction); ok {
			t.Fatal("the shell ended with the request that made it")
		}
		if d, ok := env.Action.Value.(*ahptypes.TerminalDataAction); ok && d.Data == "$ " {
			break
		}
	}
}

func TestANewSessionIsCreatedByItsFirstMessage(t *testing.T) {
	boxes := &fakeBoxes{}
	srv, ctx := startHost(t, boxes)
	client, _ := connect(ctx, t, srv)
	provider := "claude"
	uri := "ahp-session:/5d1c0b8e"
	if err := client.Request(ctx, "createSession", ahptypes.CreateSessionParams{
		Channel: uri, Provider: &provider, WorkingDirectories: []string{"file:///home/me/src/app"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	res, _, err := client.Subscribe(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	chatURI := *res.Snapshot.State.Session.DefaultChat
	if _, err := client.Dispatch(ctx, chatURI, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "t1", StartedAt: now(),
		Message: ahptypes.Message{Text: "fix the flaky test", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the create", func() bool {
		_, _, _, runs, _ := boxes.snapshot()
		return len(runs) == 1
	})
	_, _, _, runs, _ := boxes.snapshot()
	if runs[0].Harness != "claude" || runs[0].Source != "/home/me/src/app" || len(runs[0].Prompt) != 1 || runs[0].Prompt[0] != "fix the flaky test" || runs[0].IncludeDirty != "false" {
		t.Fatalf("run %+v", runs[0])
	}
	// The discobox the listing then finds is this session's, not a second.
	time.Sleep(100 * time.Millisecond)
	var list ahptypes.ListSessionsResult
	if err := client.Request(ctx, "listSessions", ahptypes.ListSessionsParams{Channel: ahptypes.RootResourceURI}, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0].Resource != uri {
		t.Fatalf("sessions %+v", list.Items)
	}
}

func TestUTF8PrefixHoldsBackAnIncompleteCharacter(t *testing.T) {
	for _, tc := range []struct {
		in         string
		text, rest string
	}{
		{"abc", "abc", ""},
		{"caf\xc3", "caf", "\xc3"},
		{"\xe2\x82", "", "\xe2\x82"},
		{"€", "€", ""},
		{"a\xff", "a\xff", ""},
	} {
		text, rest := utf8Prefix([]byte(tc.in))
		if text != tc.text || string(rest) != tc.rest {
			t.Errorf("%q: %q %q, want %q %q", tc.in, text, rest, tc.text, tc.rest)
		}
	}
}

func TestATerminalKeepsOnlyItsTail(t *testing.T) {
	state := ahptypes.TerminalState{Content: []ahptypes.TerminalContentPart{
		{Value: &ahptypes.TerminalUnclassifiedPart{Type: "unclassified", Value: strings.Repeat("x", terminalContentLimit)}},
		{Value: &ahptypes.TerminalUnclassifiedPart{Type: "unclassified", Value: "line\nrest"}},
	}}
	trimTerminal(&state)
	if len(state.Content) != 1 || partText(state.Content[0]) != "line\nrest" {
		t.Fatalf("content %+v", state.Content)
	}
	state = ahptypes.TerminalState{Content: []ahptypes.TerminalContentPart{
		{Value: &ahptypes.TerminalUnclassifiedPart{Type: "unclassified", Value: "old\n" + strings.Repeat("y", terminalContentLimit)}},
	}}
	trimTerminal(&state)
	if got := partText(state.Content[0]); len(got) > terminalContentLimit {
		t.Fatalf("kept %d bytes", len(got))
	}
}

func TestAnExecThatExitsLeavesItsTerminalExited(t *testing.T) {
	boxes := &fakeBoxes{
		boxes: []tui.Sandbox{runningBox()},
		execs: map[string][]tui.Exec{"sbx_1": {{ID: "exec-primary", Harness: "claude", Primary: true, Live: true}}},
	}
	srv, ctx := startHost(t, boxes)
	client, _ := connect(ctx, t, srv)
	if _, _, err := client.Subscribe(ctx, "ahp-session:/sbx_1"); err != nil {
		t.Fatal(err)
	}
	res, _, err := client.Subscribe(ctx, "ahp-chat:/sbx_1/primary")
	if err != nil {
		t.Fatal(err)
	}
	call, ok := screenCall(res.Snapshot.State.Chat)
	if !ok {
		t.Fatal("no screen")
	}
	_, screen := subscribeTerminal(ctx, t, client, call.Content[0].Value.(*ahptypes.ToolResultTerminalContent).Resource)
	eventually(t, "the attach", func() bool {
		opened, _, _, _, _ := boxes.snapshot()
		return len(opened) == 1
	})
	boxes.mu.Lock()
	stream := boxes.streams[0]
	boxes.mu.Unlock()

	// The attach of an exec that exited cleanly ends with io.EOF.
	stream.Close()
	for {
		env := nextAction(t, screen)
		if _, ok := env.Action.Value.(*ahptypes.TerminalExitedAction); ok {
			break
		}
	}
	time.Sleep(100 * time.Millisecond)
	if opened, _, _, _, _ := boxes.snapshot(); len(opened) != 1 {
		t.Fatalf("attached %v: an exited exec was attached again", opened)
	}
}

func TestACreatedChatIsNotListedTwice(t *testing.T) {
	boxes := &fakeBoxes{boxes: []tui.Sandbox{runningBox()}}
	srv, ctx := startHost(t, boxes)
	client, _ := connect(ctx, t, srv)
	if err := client.Request(ctx, "createChat", ahptypes.CreateChatParams{Channel: "ahp-session:/sbx_1", Chat: "ahp-chat:/client-chosen"}, nil); err != nil {
		t.Fatal(err)
	}
	// The discobox now lists the exec the chat was made for.
	boxes.mu.Lock()
	boxes.execs = map[string][]tui.Exec{"sbx_1": {
		{ID: "exec-primary", Harness: "claude", Primary: true, Live: true},
		{ID: "term-2", Harness: "claude", Live: true},
	}}
	boxes.mu.Unlock()
	// Subscribing lists the execs, and the poll after lists them again.
	if _, _, err := client.Subscribe(ctx, "ahp-session:/sbx_1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	res, _, err := client.Subscribe(ctx, "ahp-session:/sbx_1")
	if err != nil {
		t.Fatal(err)
	}
	var uris []string
	for _, chat := range res.Snapshot.State.Session.Chats {
		uris = append(uris, chat.Resource)
	}
	if len(uris) != 2 || uris[1] != "ahp-chat:/client-chosen" {
		t.Fatalf("chats %v, want the primary and the created one", uris)
	}
}

func TestAShellWhoseExecIsGoneIsExitedNotRetried(t *testing.T) {
	boxes := &fakeBoxes{boxes: []tui.Sandbox{runningBox()}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	host := New(Config{Boxes: boxes, Token: testToken, Poll: 20 * time.Millisecond, Logf: t.Logf})
	go func() { _ = host.Run(ctx) }()
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	client, _ := connect(ctx, t, srv)
	cwd := "file://sbx_1/home/me/src/app"
	if err := client.Request(ctx, "createTerminal", ahptypes.CreateTerminalParams{
		Channel: "agenthost-terminal:/t1", Cwd: &cwd,
		Claim: ahptypes.TerminalClaim{Value: &ahptypes.TerminalClientClaim{Kind: ahptypes.TerminalClaimKindClient, ClientId: "client-1"}},
	}, nil); err != nil {
		t.Fatal(err)
	}
	// Made long enough ago that a listing without its exec means it is gone:
	// the discobox restarted, and the shell with it.
	host.mu.Lock()
	host.terminals["agenthost-terminal:/t1"].created = time.Now().Add(-2 * createdGrace)
	host.mu.Unlock()
	eventually(t, "the shell to be exited", func() bool {
		host.mu.Lock()
		defer host.mu.Unlock()
		return host.terminals["agenthost-terminal:/t1"].exited()
	})
}

func TestAListingOutlivesThePollThatAsked(t *testing.T) {
	boxes := &fakeBoxes{boxes: []tui.Sandbox{runningBox()}}
	srv, ctx := startHost(t, boxes)
	connect(ctx, t, srv)
	time.Sleep(200 * time.Millisecond)
	boxes.mu.Lock()
	defer boxes.mu.Unlock()
	if boxes.listingCut {
		t.Fatal("a listing's context ended with the poll: a slow server's request would be canceled")
	}
}

// failingStart is fakeBoxes whose discoboxes cannot be started.
type failingStart struct{ *fakeBoxes }

func (f failingStart) Do(ctx context.Context, verb tui.Verb, id string) error {
	_ = f.fakeBoxes.Do(ctx, verb, id)
	return errors.New("cannot start sandbox: it has no runtime on its pool")
}

func TestAMessageToADiscoboxThatCannotStartSaysSo(t *testing.T) {
	box := runningBox()
	box.State = tui.StateError
	boxes := &fakeBoxes{boxes: []tui.Sandbox{box}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	host := New(Config{Boxes: failingStart{boxes}, Token: testToken, Poll: 20 * time.Millisecond, Logf: t.Logf})
	go func() { _ = host.Run(ctx) }()
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	client, _ := connect(ctx, t, srv)
	chatURI := "ahp-chat:/sbx_1/primary"
	res, sub, err := client.Subscribe(ctx, chatURI)
	if err != nil {
		t.Fatal(err)
	}
	state := *res.Snapshot.State.Chat
	if _, err := client.Dispatch(ctx, chatURI, ahptypes.StateAction{Value: &ahptypes.ChatPendingMessageSetAction{
		Type: ahptypes.ActionTypeChatPendingMessageSet, Kind: ahptypes.PendingMessageKindQueued, Id: "q1",
		Message: ahptypes.Message{Text: "hello", Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindUser}},
	}}); err != nil {
		t.Fatal(err)
	}
	for {
		ahp.ApplyActionToChat(&state, nextAction(t, sub).Action)
		if state.ActiveTurn == nil {
			continue
		}
		var said string
		for _, part := range state.ActiveTurn.ResponseParts {
			if md, ok := part.Value.(*ahptypes.MarkdownResponsePart); ok {
				said = md.Content
			}
		}
		if said != "" {
			if !strings.Contains(said, "no runtime") {
				t.Fatalf("turn says %q", said)
			}
			break
		}
	}
	time.Sleep(100 * time.Millisecond)
	if _, _, verbs, _, _ := boxes.snapshot(); len(verbs) != 1 {
		t.Fatalf("starts %v, want one", verbs)
	}
}
