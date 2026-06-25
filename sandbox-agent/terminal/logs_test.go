package terminal

import (
	"context"
	"testing"
	"time"
)

func TestAsyncLoggerWritesStructuredEntries(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewAsyncLogger(dir, "agt_test")
	if err != nil {
		t.Fatal(err)
	}
	logger.Record(LogStreamOutput, []byte("hello\n"))
	logger.Record(LogStreamInput, []byte("pwd\n"))
	logger.Close()

	entries, err := ReadLogs(context.Background(), dir, "agt_test")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Stream != LogStreamOutput || string(entries[0].Data) != "hello\n" {
		t.Fatalf("first entry = %#v/%q, want output hello", entries[0].Stream, entries[0].Data)
	}
	if entries[1].Stream != LogStreamInput || string(entries[1].Data) != "pwd\n" {
		t.Fatalf("second entry = %#v/%q, want input pwd", entries[1].Stream, entries[1].Data)
	}
}

func TestLogBucketRoundsToNearestFifteenSeconds(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		in   time.Time
		want time.Time
	}{
		{name: "down", in: base.Add(7 * time.Second), want: base},
		{name: "up", in: base.Add(8 * time.Second), want: base.Add(15 * time.Second)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := logBucket(tt.in); !got.Equal(tt.want) {
				t.Fatalf("logBucket(%s) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}
