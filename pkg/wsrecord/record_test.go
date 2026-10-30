package wsrecord

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"
)

func TestRecordsAndTruncation(t *testing.T) {
	var file bytes.Buffer
	if err := WriteHeader(&file); err != nil {
		t.Fatal(err)
	}
	for _, opcode := range []byte{Text, Binary, CaptureError} {
		if err := Write(&file, Record{Time: time.Unix(1, 2), Direction: ClientToServer, Opcode: opcode, Payload: []byte{0, 255, 42}}); err != nil {
			t.Fatal(err)
		}
	}
	data := file.Bytes()
	r := bytes.NewReader(data)
	if err := ReadHeader(r); err != nil {
		t.Fatal(err)
	}
	for _, opcode := range []byte{Text, Binary, CaptureError} {
		got, err := Read(r)
		if err != nil || got.Opcode != opcode || !got.Time.Equal(time.Unix(1, 2)) || !bytes.Equal(got.Payload, []byte{0, 255, 42}) {
			t.Fatalf("record %v err %v", got, err)
		}
	}
	if _, err := Read(r); !errors.Is(err, io.EOF) {
		t.Fatalf("end=%v", err)
	}
	for n := HeaderSize; n <= len(data); n++ {
		cut, err := Cut(bytes.NewReader(data[:n]), int64(n))
		want := int64(HeaderSize + (n-HeaderSize)/(RecordHeaderSize+3)*(RecordHeaderSize+3))
		if err != nil || cut != want {
			t.Fatalf("n=%d cut=%d want=%d err=%v", n, cut, want, err)
		}
	}
}
func TestReaderRejectsOversizedPayload(t *testing.T) {
	var header [RecordHeaderSize]byte
	binary.BigEndian.PutUint32(header[10:], MaxPayload+1)
	if _, err := Read(bytes.NewReader(header[:])); err == nil {
		t.Fatal("accepted oversized record")
	}
}

func TestPayloadMissingAfterWholeHeaderIsTruncation(t *testing.T) {
	var header [RecordHeaderSize]byte
	binary.BigEndian.PutUint32(header[10:], 1)
	if _, err := Read(bytes.NewReader(header[:])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("missing payload error=%v", err)
	}
}
