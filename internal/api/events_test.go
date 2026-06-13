package api_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProjectEventsListWatchSnapshot(t *testing.T) {
	h := newTestAPI(t).h
	created := createSandbox(t, h, "alpha")

	resp := h.Get(projectURL() + "/events?resources=sandbox&list=true&replayOnly=true")
	if resp.Code != http.StatusOK {
		t.Fatalf("events status = %d, body = %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !containsAll(body, "event: listStart", "event: resourceListed", "event: listFinish", `"resourceType":"sandbox"`, `"resourceId":"`+created.ID+`"`, `"action":"listed"`) {
		t.Fatalf("unexpected events body: %s", body)
	}

	resp = h.Get(projectURL() + "/events?afterSeq=0&replayOnly=true")
	if resp.Code != http.StatusOK {
		t.Fatalf("events afterSeq=0 status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if resp.Body.Len() != 0 {
		t.Fatalf("expected no replayed events when afterSeq=0 starts at max seq, got: %s", resp.Body.String())
	}
}

func TestProjectEventsListWatchSnapshotIncludesAgentConfigs(t *testing.T) {
	h := newTestAPI(t).h
	config := createAgentConfig(t, h, "Codex")

	resp := h.Get(projectURL() + "/events?resources=agentConfig&list=true&replayOnly=true")
	if resp.Code != http.StatusOK {
		t.Fatalf("events status = %d, body = %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !containsAll(body, "event: listStart", "event: resourceListed", "event: listFinish", `"resourceType":"agentConfig"`, `"resourceId":"`+config.ID+`"`, `"action":"listed"`, `"name":"Codex"`) {
		t.Fatalf("unexpected events body: %s", body)
	}
}

func TestProjectEventsReplayAgentConfigChanges(t *testing.T) {
	h := newTestAPI(t).h
	config := createAgentConfig(t, h, "Codex")

	resp := h.Patch(projectURL()+"/agent-configs/"+config.ID, map[string]any{
		"name": "Codex Updated",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("update agent config status = %d, body = %s", resp.Code, resp.Body.String())
	}
	resp = h.Delete(projectURL() + "/agent-configs/" + config.ID)
	if resp.Code != http.StatusNoContent {
		t.Fatalf("delete agent config status = %d, body = %s", resp.Code, resp.Body.String())
	}

	resp = h.Get(projectURL() + "/events?resources=agentConfig&afterSeq=1&replayOnly=true")
	if resp.Code != http.StatusOK {
		t.Fatalf("events replay status = %d, body = %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !containsAll(body, "event: resourceChanged", `"resourceType":"agentConfig"`, `"resourceId":"`+config.ID+`"`, `"action":"updated"`, `"action":"deleted"`, `"name":"Codex Updated"`) {
		t.Fatalf("unexpected replay body: %s", body)
	}
}

func TestProjectEventsReplayAfterSeq(t *testing.T) {
	h := newTestAPI(t).h
	created := createSandbox(t, h, "alpha")

	resp := h.Patch(sandboxURL(created.ID), map[string]any{
		"name": "beta",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("update sandbox status = %d, body = %s", resp.Code, resp.Body.String())
	}

	resp = h.Get(projectURL() + "/events?resources=sandbox&afterSeq=1&replayOnly=true")
	if resp.Code != http.StatusOK {
		t.Fatalf("events replay status = %d, body = %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !containsAll(body, "event: resourceChanged", `"resourceType":"sandbox"`, `"resourceId":"`+created.ID+`"`, `"action":"updated"`, `"name":"beta"`) {
		t.Fatalf("unexpected replay body: %s", body)
	}
}

func TestProjectEventsResourceFilterRepeatedAndCommaSeparated(t *testing.T) {
	h := newTestAPI(t).h
	created := createSandbox(t, h, "alpha")

	resp := h.Get(projectURL() + "/events?resources=unknown,sandbox&resources=other&list=true&replayOnly=true")
	if resp.Code != http.StatusOK {
		t.Fatalf("events status = %d, body = %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !containsAll(body, "event: listStart", "event: resourceListed", "event: listFinish", `"resourceId":"`+created.ID+`"`) {
		t.Fatalf("expected sandbox listed with mixed resource filters, got: %s", body)
	}

	resp = h.Get(projectURL() + "/events?resources=unknown&list=true&replayOnly=true")
	if resp.Code != http.StatusOK {
		t.Fatalf("unknown-only events status = %d, body = %s", resp.Code, resp.Body.String())
	}
	body = resp.Body.String()
	if strings.Contains(body, "event: resourceListed") {
		t.Fatalf("did not expect listed resources for unknown-only filter, got: %s", body)
	}
}

func TestProjectEventsReconnectFromObservedSeq(t *testing.T) {
	h := newTestAPI(t).h
	created := createSandbox(t, h, "alpha")
	updateSandboxName(t, h, created.ID, "beta")
	updateSandboxName(t, h, created.ID, "gamma")

	resp := h.Get(projectURL() + "/events?resources=sandbox&afterSeq=2&replayOnly=true")
	if resp.Code != http.StatusOK {
		t.Fatalf("events reconnect status = %d, body = %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !containsAll(body, "event: resourceChanged", `"action":"updated"`, `"name":"gamma"`) {
		t.Fatalf("expected reconnect replay to include later update, got: %s", body)
	}
	if strings.Contains(body, `"name":"beta"`) || strings.Contains(body, `"action":"created"`) {
		t.Fatalf("reconnect replay included already-observed events: %s", body)
	}
}

func TestProjectEventsLiveSubscriptionReceivesMutation(t *testing.T) {
	apiFixture := newTestAPI(t)
	server := httptest.NewServer(apiFixture.handler)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+projectURL()+"/events?resources=sandbox&list=true", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("event stream status = %d", resp.StatusCode)
	}

	time.Sleep(50 * time.Millisecond)
	created := createSandbox(t, apiFixture.h, "live")
	event := readSSEUntil(t, resp.Body, "event: resourceChanged")
	if !containsAll(event, "event: resourceChanged", `"resourceId":"`+created.ID+`"`, `"action":"created"`) {
		t.Fatalf("unexpected live event: %s", event)
	}
}

func updateSandboxName(t *testing.T, h interface {
	Patch(path string, args ...any) *httptest.ResponseRecorder
}, sandboxID, name string) {
	t.Helper()

	resp := h.Patch(sandboxURL(sandboxID), map[string]any{"name": name})
	if resp.Code != http.StatusOK {
		t.Fatalf("update sandbox status = %d, body = %s", resp.Code, resp.Body.String())
	}
}

func readSSEUntil(t *testing.T, body io.Reader, needle string) string {
	t.Helper()

	reader := bufio.NewReader(body)
	for {
		event := readSSEEvent(t, reader)
		if strings.Contains(event, needle) {
			return event
		}
	}
}

func readSSEEvent(t *testing.T, reader *bufio.Reader) string {
	t.Helper()

	var lines []string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read event stream: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" && len(lines) > 0 {
			return strings.Join(lines, "\n")
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
}
