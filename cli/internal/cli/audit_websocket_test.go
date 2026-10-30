package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/wsrecord"
)

func TestConsoleReadsWebSocketRecording(t *testing.T) {
	var file bytes.Buffer
	_ = wsrecord.WriteHeader(&file)
	for _, record := range []wsrecord.Record{
		{Time: time.Unix(1, 0), Direction: wsrecord.ClientToServer, Opcode: wsrecord.Text, Payload: []byte("hello\x1b[2J")},
		{Time: time.Unix(2, 0), Direction: wsrecord.ServerToClient, Opcode: wsrecord.Binary, Payload: []byte{0, 255}},
		{Time: time.Unix(3, 0), Direction: wsrecord.ServerToClient, Opcode: wsrecord.CaptureError, Payload: []byte("capture stopped")},
	} {
		if err := wsrecord.Write(&file, record); err != nil {
			t.Fatal(err)
		}
	}
	ds := auditBodyServer(t, map[string]string{"stream": file.String()})
	got, err := ds.AuditBody(t.Context(), "sbx_1", "http_42", httpAuditPartStream)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sent  text", "received  binary", "hello\\x1b[2J", "00ff", "capture stopped"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
	if strings.ContainsRune(got, 27) {
		t.Fatal("unescaped terminal instruction")
	}
	var out bytes.Buffer
	if err := renderWebSocketRecording(&out, bytes.NewReader(file.Bytes()[:file.Len()-1])); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "capture truncated") {
		t.Fatalf("no truncation diagnostic: %s", out.String())
	}
}
