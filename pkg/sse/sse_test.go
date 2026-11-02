package sse

import (
	"errors"
	"strings"
	"testing"
)

func TestRead(t *testing.T) {
	stream := "retry: 500\n\n: comment\nid: 4\nevent: sandbox.updated\ndata: {\"a\":\ndata: 1}\n\nevent: reset\ndata: {}\n\nid: 9\n\ndata: plain\n\n"
	var got []string
	var fields []string
	err := Read(strings.NewReader(stream), func(field, value string) {
		fields = append(fields, field+"="+value)
	}, func(id, event, data string) error {
		got = append(got, id+"|"+event+"|"+data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"4|sandbox.updated|{\"a\":\n1}", "|reset|{}", "9||", "|message|plain"}
	if strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if strings.Join(fields, ",") != "retry=500" {
		t.Fatalf("fields = %q", fields)
	}
}

// An error from onEvent ends the read with it, and an unfinished block at the
// stream's end is not an event.
func TestReadStopsOnTheCallersError(t *testing.T) {
	stop := errors.New("stop")
	calls := 0
	err := Read(strings.NewReader("id: 1\nevent: a\n\nid: 2\nevent: b\n\nid: 3\nevent: c"), nil, func(string, string, string) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("err = %v after %d calls, want stop after 1", err, calls)
	}
	calls = 0
	if err := Read(strings.NewReader("id: 3\nevent: c"), nil, func(string, string, string) error { calls++; return nil }); err != nil || calls != 0 {
		t.Fatalf("an unfinished block: err %v, %d calls", err, calls)
	}
}
