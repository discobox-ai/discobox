package protocol

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestInitializeAndListSessions(t *testing.T) {
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	errCh := make(chan error, 1)
	go func() {
		conn, err := serverTransport.Connect(context.Background())
		if err != nil {
			errCh <- err
			return
		}
		defer conn.Close()
		for {
			msg, err := conn.Read(context.Background())
			if err != nil {
				errCh <- nil
				return
			}
			req, ok := msg.(*jsonrpc.Request)
			if !ok {
				continue
			}
			var result any
			switch req.Method {
			case "initialize":
				result = InitializeResponse{
					ProtocolVersion: ProtocolVersion,
					AgentCapabilities: AgentCapabilities{SessionCapabilities: SessionCapabilities{
						List: &map[string]any{},
					}},
				}
			case "session/list":
				result = ListSessionsResponse{Sessions: []SessionInfo{{SessionID: "s1", CWD: "/tmp/work"}}}
			default:
				t.Errorf("unexpected method %q", req.Method)
				result = map[string]any{}
			}
			raw, err := json.Marshal(result)
			if err != nil {
				errCh <- err
				return
			}
			if err := conn.Write(context.Background(), &jsonrpc.Response{ID: req.ID, Result: raw}); err != nil {
				errCh <- err
				return
			}
		}
	}()

	client, err := New(t.Context(), clientTransport)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	init, err := client.Initialize(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !init.SupportsSessionList() {
		t.Fatal("expected session/list support")
	}
	sessions, err := client.ListSessions(t.Context(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions.Sessions) != 1 || sessions.Sessions[0].SessionID != "s1" {
		t.Fatalf("sessions = %#v", sessions.Sessions)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	default:
	}
}
