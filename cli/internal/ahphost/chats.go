package ahphost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"

	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// chat is one of a discobox's harness terminals (ADR 26-10-17-354 §3). Its
// conversation is not Discobox's to see, so a turn is a message the user sent
// — typed into the terminal — and the one tool call under it is the terminal
// itself, live: `discobox attach --raw`.
//
// The turn is left running. VS Code attaches a terminal it can type into only
// for a tool call it watches while it runs, and it queues a message sent
// meanwhile instead of sending it, so a queued message is what ends one turn
// and starts the next (handleChatAction).
type chat struct {
	uri     string
	s       *session
	primary bool
	// execID is the harness terminal's exec, empty for the primary until the
	// discobox's execs have been listed.
	execID string
	state  ahptypes.ChatState
	subs   map[*conn]struct{}
	// screen is the terminal the running turn shows. A turn started after it
	// exited gets a new one.
	screen *terminal
	turns  int
	// created is when a client's createChat made this chat, which an exec
	// listing taken before it does not have yet.
	created time.Time
}

func (c *chat) summary() ahptypes.ChatSummary {
	return ahptypes.ChatSummary{
		Resource:   c.uri,
		Title:      c.state.Title,
		Status:     c.state.Status,
		Activity:   c.state.Activity,
		ModifiedAt: c.state.ModifiedAt,
	}
}

// inputExec is the exec a message is typed into. The primary is addressed by
// the sandbox's virtual "primary" id, which relaunches a harness that has
// stopped: sending it a message is asking for it (ADR 26-10-17-354 §5).
func (c *chat) inputExec() string {
	if c.primary {
		return "primary"
	}
	return c.execID
}

// newChatLocked makes a chat with its first turn already running, so a client
// opening it attaches its screen at once. h.mu must be held.
func (h *Host) newChatLocked(s *session, uri string, primary bool, execID string) *chat {
	c := &chat{uri: uri, s: s, primary: primary, execID: execID, subs: map[*conn]struct{}{}}
	title := "Terminal"
	if primary {
		title = "Primary terminal"
	}
	c.state = ahptypes.ChatState{
		Resource:   uri,
		Title:      title,
		Status:     ahptypes.SessionStatusIdle,
		ModifiedAt: now(),
		Turns:      []ahptypes.Turn{},
	}
	h.chats[uri] = c
	h.startTurnLocked(c, ahptypes.Message{
		Text:   "discobox attach --raw " + s.key,
		Origin: ahptypes.MessageOrigin{Kind: ahptypes.MessageKindSystemNotification},
	}, nil, nil)
	return c
}

// startTurnLocked starts a turn for message and puts the chat's screen under
// it. origin is the client's turnStarted this echoes; queued is the queued
// message it consumes.
func (h *Host) startTurnLocked(c *chat, message ahptypes.Message, origin *ahptypes.ActionOrigin, queued *string) string {
	c.turns++
	turnID := fmt.Sprintf("turn-%d", c.turns)
	h.dispatch(c.uri, ahptypes.StateAction{Value: &ahptypes.ChatTurnStartedAction{
		Type: ahptypes.ActionTypeChatTurnStarted, TurnId: turnID, StartedAt: now(), Message: message, QueuedMessageId: queued,
	}}, origin)
	h.showScreenLocked(c, turnID)
	return turnID
}

// showScreenLocked adds the chat's screen to a turn as a running terminal
// tool call. The command line is what the tool call shows; VS Code renders a
// terminal tool call only once it has one.
func (h *Host) showScreenLocked(c *chat, turnID string) {
	if c.screen == nil || c.screen.exited() {
		c.screen = h.newScreenLocked(c)
	}
	callID := "screen-" + turnID
	meta := map[string]json.RawMessage{"toolKind": json.RawMessage(`"terminal"`)}
	h.dispatch(c.uri, ahptypes.StateAction{Value: &ahptypes.ChatToolCallStartAction{
		Type: ahptypes.ActionTypeChatToolCallStart, TurnId: turnID, ToolCallId: callID,
		ToolName: "discobox_attach", DisplayName: "Discobox terminal", Meta: meta,
	}}, nil)
	command := "discobox attach --raw " + c.s.key
	h.dispatch(c.uri, ahptypes.StateAction{Value: &ahptypes.ChatToolCallReadyAction{
		Type: ahptypes.ActionTypeChatToolCallReady, TurnId: turnID, ToolCallId: callID, Meta: meta,
		InvocationMessage: ahptypes.NewStringOrMarkdownPlain("Attached to " + c.state.Title),
		ToolInput:         &ahptypes.ToolInput{Inline: &command},
		Confirmed:         ptr(ahptypes.ToolCallConfirmationReasonNotNeeded),
	}}, nil)
	h.dispatch(c.uri, ahptypes.StateAction{Value: &ahptypes.ChatToolCallContentChangedAction{
		Type: ahptypes.ActionTypeChatToolCallContentChanged, TurnId: turnID, ToolCallId: callID,
		Content: []ahptypes.ToolResultContent{c.screen.content(c.state.Title)},
	}}, nil)
}

// endTurnLocked completes the running turn, its screen with it. The terminal
// goes on running; the tool call records only that this turn is over.
func (h *Host) endTurnLocked(c *chat) {
	turn := c.state.ActiveTurn
	if turn == nil {
		return
	}
	callID := "screen-" + turn.Id
	content := []ahptypes.ToolResultContent{}
	if c.screen != nil {
		content = append(content, c.screen.content(c.state.Title))
	}
	h.dispatch(c.uri, ahptypes.StateAction{Value: &ahptypes.ChatToolCallCompleteAction{
		Type: ahptypes.ActionTypeChatToolCallComplete, TurnId: turn.Id, ToolCallId: callID,
		Result: ahptypes.ToolCallResult{Success: true, PastTenseMessage: ahptypes.NewStringOrMarkdownPlain("Attached to " + c.state.Title), Content: content},
	}}, nil)
	h.dispatch(c.uri, ahptypes.StateAction{Value: &ahptypes.ChatTurnCompleteAction{
		Type: ahptypes.ActionTypeChatTurnComplete, TurnId: turn.Id, Duration: sinceMillis(turn.StartedAt),
	}}, nil)
}

func sinceMillis(started string) int64 {
	t, err := time.Parse(time.RFC3339Nano, started)
	if err != nil {
		return 0
	}
	return time.Since(t).Milliseconds()
}

// syncChatSummaryLocked keeps the session's catalog entry for a chat equal
// to the chat's own state, as the spec requires of the chat's producer.
func (h *Host) syncChatSummaryLocked(c *chat) {
	for _, entry := range c.s.state.Chats {
		if entry.Resource != c.uri {
			continue
		}
		if entry.Status == c.state.Status && entry.Title == c.state.Title && entry.ModifiedAt == c.state.ModifiedAt {
			return
		}
		break
	}
	h.dispatch(c.s.uri, ahptypes.StateAction{Value: &ahptypes.SessionChatUpdatedAction{
		Type: ahptypes.ActionTypeSessionChatUpdated, Chat: c.uri,
		Changes: ahptypes.PartialChatSummary{Title: &c.state.Title, Status: &c.state.Status, ModifiedAt: &c.state.ModifiedAt},
	}}, nil)
	h.publishSummaryLocked(c.s, h.summaryLocked(c.s))
}

// handleChatAction is a client's action on a chat. A message — a turn the
// client started, or one it queued behind the running turn — is typed into
// the terminal; everything else is the client's own state and is applied and
// echoed as it came.
func (h *Host) handleChatAction(c *chat, act ahptypes.StateAction, origin *ahptypes.ActionOrigin) {
	switch a := act.Value.(type) {
	case *ahptypes.ChatTurnStartedAction:
		// A turn the client starts replaces the running one in every reducer,
		// so that one is ended first.
		h.endTurnLocked(c)
		h.dispatch(c.uri, act, origin)
		h.showScreenLocked(c, a.TurnId)
		h.syncChatSummaryLocked(c)
		go h.deliver(c, a.TurnId, a.Message)
	case *ahptypes.ChatPendingMessageSetAction:
		h.dispatch(c.uri, act, origin)
		h.endTurnLocked(c)
		turnID := h.startTurnLocked(c, a.Message, nil, &a.Id)
		h.syncChatSummaryLocked(c)
		go h.deliver(c, turnID, a.Message)
	default:
		h.dispatch(c.uri, act, origin)
		h.syncChatSummaryLocked(c)
	}
}

// deliverTimeout bounds how long a message waits for its discobox: a create,
// or a start and the harness coming up.
const deliverTimeout = 10 * time.Minute

// deliver types a message into the chat's terminal. A session with no
// discobox yet is created with the message as its prompt; a discobox that is
// stopped is started first. What fails is said in the turn.
func (h *Host) deliver(c *chat, turnID string, message ahptypes.Message) {
	ctx, cancel := context.WithTimeout(h.ctx, deliverTimeout)
	defer cancel()
	err := h.deliverMessage(ctx, c, message)
	if err == nil {
		return
	}
	h.cfg.Logf("ahp: deliver a message to %s: %v", c.s.key, err)
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.state.ActiveTurn == nil || c.state.ActiveTurn.Id != turnID {
		return
	}
	h.dispatch(c.uri, ahptypes.StateAction{Value: &ahptypes.ChatResponsePartAction{
		Type: ahptypes.ActionTypeChatResponsePart, TurnId: turnID,
		Part: ahptypes.ResponsePart{Value: &ahptypes.MarkdownResponsePart{
			Kind: ahptypes.ResponsePartKindMarkdown, Id: "error-" + turnID, Content: "The message was not delivered: " + err.Error(),
		}},
	}}, nil)
}

func (h *Host) deliverMessage(ctx context.Context, c *chat, message ahptypes.Message) error {
	h.mu.Lock()
	s := c.s
	if !s.hasBox {
		if s.creating {
			h.mu.Unlock()
			return fmt.Errorf("the discobox is still being created")
		}
		s.creating = true
		h.refreshSessionStateLocked(s)
		h.mu.Unlock()
		if err := h.create(ctx, s, message.Text); err != nil {
			return err
		}
		// The text was the prompt the discobox was created with. Its images
		// have nowhere to go until the discobox runs, and then go in as a
		// message of their own, their paths typed.
		message.Text = ""
		if !hasImage(message.Attachments) {
			return nil
		}
	} else {
		h.mu.Unlock()
	}
	if err := h.awaitRunning(ctx, s); err != nil {
		return err
	}
	h.mu.Lock()
	sandboxID, execID := s.box.ID, c.inputExec()
	h.mu.Unlock()
	if execID == "" {
		return fmt.Errorf("the terminal is not known yet")
	}
	text := message.Text
	for _, path := range h.putAttachments(ctx, sandboxID, message.Attachments) {
		text = strings.TrimSpace(text + " " + path)
	}
	if text == "" {
		return nil
	}
	return h.cfg.Boxes.SendInput(ctx, sandboxID, execID, text)
}

// create makes a created session's discobox, its first message as the prompt.
func (h *Host) create(ctx context.Context, s *session, prompt string) error {
	h.mu.Lock()
	req := tui.RunRequest{Harness: s.harness, Source: s.source, IncludeDirty: "false", Detach: true}
	h.mu.Unlock()
	if text := strings.TrimSpace(prompt); text != "" {
		req.Prompt = []string{text}
	}
	box, err := h.cfg.Boxes.Run(ctx, req, func(line string) { h.cfg.Logf("ahp: %s: %s", s.key, line) }, nil)
	h.mu.Lock()
	defer h.mu.Unlock()
	s.creating = false
	if err != nil {
		h.refreshSessionStateLocked(s)
		return err
	}
	if other, ok := h.boxSessions[box.ID]; ok && other != s.uri {
		// The listing found it first, under its own URI; this session takes
		// its place.
		if listed := h.sessions[other]; listed != nil {
			h.removeSessionLocked(listed)
		}
	}
	s.box, s.hasBox, s.boxCreated = box, true, time.Now()
	h.boxSessions[box.ID] = s.uri
	h.refreshSessionStateLocked(s)
	h.addSessionLocked(s)
	h.refreshAgentsLocked()
	h.kick()
	return nil
}

// awaitRunning waits for a discobox to be running, starting it if it is
// stopped.
func (h *Host) awaitRunning(ctx context.Context, s *session) error {
	started := false
	for {
		h.mu.Lock()
		state, hasBox := s.box.State, s.hasBox
		h.mu.Unlock()
		if !hasBox {
			return fmt.Errorf("the discobox is gone")
		}
		switch state {
		case tui.StateRunning:
			return nil
		case tui.StateArchived:
			return fmt.Errorf("the discobox is archived")
		case tui.StateStopped, tui.StateError:
			// Asked once, and its answer is the delivery's: a discobox that
			// cannot start — one with no runtime, a failed create — will not
			// start on the next poll either.
			if !started {
				started = true
				if err := h.start(ctx, s); err != nil {
					return fmt.Errorf("start the discobox: %w", err)
				}
			}
		}
		h.kick()
		select {
		case <-ctx.Done():
			return fmt.Errorf("the discobox did not start: %w", ctx.Err())
		case <-time.After(h.cfg.Poll):
		}
	}
}

// start asks for a stopped discobox to be started, once until it has, and is
// the one place that does: a message waits on its answer (awaitRunning), and
// typing logs it (handleTerminalAction). A start already asked for answers
// nil; one that fails is the caller's error, and the next caller asks again.
func (h *Host) start(ctx context.Context, s *session) error {
	h.mu.Lock()
	if s.starting || !s.hasBox {
		h.mu.Unlock()
		return nil
	}
	s.starting = true
	id := s.box.ID
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	err := h.cfg.Boxes.Do(ctx, tui.VerbStart, id)
	if err != nil {
		h.mu.Lock()
		s.starting = false
		h.mu.Unlock()
	}
	h.kick()
	return err
}

// putAttachments carries a message's images into the discobox, as the
// console carries a pasted one (ADR 26-10-16-535), and says where they are.
// Anything else attached is not Discobox's to deliver and is left out.
func (h *Host) putAttachments(ctx context.Context, sandboxID string, attachments []ahptypes.MessageAttachment) []string {
	var paths []string
	for i, attachment := range attachments {
		embedded, ok := attachment.Value.(*ahptypes.MessageEmbeddedResourceAttachment)
		if !ok || !hasImage([]ahptypes.MessageAttachment{attachment}) {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(embedded.Data)
		if err != nil {
			continue
		}
		name := fmt.Sprintf("ahp-%d-%d.%s", time.Now().Unix(), i, imageExtension(embedded.ContentType))
		path, err := h.cfg.Boxes.PutImage(ctx, sandboxID, name, data)
		if err != nil {
			h.cfg.Logf("ahp: %v", err)
			continue
		}
		paths = append(paths, path)
	}
	return paths
}

// hasImage is whether any of attachments is an image putAttachments carries.
func hasImage(attachments []ahptypes.MessageAttachment) bool {
	for _, attachment := range attachments {
		if embedded, ok := attachment.Value.(*ahptypes.MessageEmbeddedResourceAttachment); ok && strings.HasPrefix(embedded.ContentType, "image/") {
			return true
		}
	}
	return false
}

func imageExtension(contentType string) string {
	switch contentType {
	case "image/jpeg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	default:
		return "png"
	}
}
