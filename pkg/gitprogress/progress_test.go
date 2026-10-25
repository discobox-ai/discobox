package gitprogress

import (
	"strconv"
	"strings"
	"testing"
)

// git redraws a meter in place with carriage returns and ends it with a
// newline; every redraw is a report, the stage resets the counts, and the
// bytes received outlast the stage that received them.
func TestProgressWriterReadsGitsMeters(t *testing.T) {
	var got []Progress
	w := &Writer{Report: func(p Progress) { got = append(got, p) }}
	stderr := "Cloning into '/w/clone'...\n" +
		"remote: Enumerating objects: 35477, done.\n" +
		"remote: Counting objects:  50% (10/20)\rremote: Counting objects: 100% (20/20), done.\n" +
		"Receiving objects:   1% (355/35477)\rReceiving objects:  50% (17739/35477), 14.90 MiB | 5.00 MiB/s\r" +
		"Receiving objects: 100% (35477/35477), 27.60 MiB | 5.00 MiB/s, done.\n" +
		"Resolving deltas:  10% (25"
	// Split mid-line, as a pipe may.
	for _, chunk := range []string{stderr[:40], stderr[40:]} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Write([]byte("76/25762)\n")); err != nil {
		t.Fatal(err)
	}
	want := []Progress{
		{Stage: Counting, Objects: 35477},
		{Stage: Counting, Objects: 10, ObjectsTotal: 20},
		{Stage: Counting, Objects: 20, ObjectsTotal: 20},
		{Stage: Receiving, Objects: 355, ObjectsTotal: 35477},
		{Stage: Receiving, Objects: 17739, ObjectsTotal: 35477, Bytes: progressBytes("14.90", "MiB")},
		{Stage: Receiving, Objects: 35477, ObjectsTotal: 35477, Bytes: progressBytes("27.60", "MiB")},
		{Stage: Resolving, Objects: 2576, ObjectsTotal: 25762, Bytes: progressBytes("27.60", "MiB")},
	}
	if len(got) != len(want) {
		t.Fatalf("reports = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("report %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if messages := w.String(); messages != "Cloning into '/w/clone'...\n" {
		t.Fatalf("messages = %q, want only git's own words, the meters left out", messages)
	}
}

// A failed clone is explained by what git said, not by its meters.
func TestProgressWriterKeepsMessagesForTheError(t *testing.T) {
	w := &Writer{}
	_, _ = w.Write([]byte("Receiving objects:  50% (1/2)\rfatal: early EOF"))
	if got := w.String(); got != "fatal: early EOF\n" {
		t.Fatalf("messages = %q", got)
	}
}

// git's fatal line is the last it writes, so a clone that said a great deal
// first still fails with it; a meter the parser does not know, redrawn in
// place, is kept once rather than crowding it out.
func TestProgressWriterKeepsTheEndOfALongStderr(t *testing.T) {
	w := &Writer{}
	for i := range 2000 {
		_, _ = w.Write([]byte("Checking connectivity: " + strconv.Itoa(i) + "\r"))
		_, _ = w.Write([]byte("Checking connectivity: " + strconv.Itoa(i) + "\r"))
	}
	_, _ = w.Write([]byte("fatal: the remote end hung up unexpectedly\n"))
	got := w.String()
	if !strings.HasSuffix(got, "Checking connectivity: 1999\nfatal: the remote end hung up unexpectedly\n") || len(got) > maxCloneMessages {
		t.Fatalf("messages (%d bytes) end %q, want the fatal line kept under the bound", len(got), got[max(0, len(got)-120):])
	}
	if strings.Count(got, "Checking connectivity: 1999\n") != 1 || !strings.HasPrefix(got, "Checking connectivity: ") {
		t.Fatalf("messages = %q, want each redraw once and whole lines only", got[:min(len(got), 120)])
	}
}

func TestUnfinishedRemoteLineIsBounded(t *testing.T) {
	writer := &Writer{}
	for range 256 {
		_, _ = writer.Write([]byte(strings.Repeat("x", 4096)))
	}
	if len(writer.partial) > maxCloneMessages || cap(writer.partial) > 2*maxCloneMessages {
		t.Fatalf("unfinished remote line retained %d bytes (capacity %d)", len(writer.partial), cap(writer.partial))
	}
	_, _ = writer.Write([]byte("\nfatal: remote disconnected\n"))
	if text := writer.String(); len(text) > maxCloneMessages || !strings.HasSuffix(text, "fatal: remote disconnected\n") {
		t.Fatal("bounded stderr lost its final error")
	}
}
