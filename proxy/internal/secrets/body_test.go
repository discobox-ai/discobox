package secrets

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// What an authorizer reads is handed back in front of the rest, so the
// upstream receives the body the sandbox sent whatever size it was.
func TestACapturedBodyIsSentWhole(t *testing.T) {
	for _, tc := range []struct {
		name     string
		size     int
		complete bool
	}{
		{"shorter than the cap", 100, true},
		{"exactly the cap", MaxCapturedBody, true},
		{"one byte past the cap", MaxCapturedBody + 1, false},
		{"far past the cap", 3*MaxCapturedBody + 7, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent := bytes.Repeat([]byte("0123456789abcdef"), tc.size/16+1)[:tc.size]
			body := NewRequestBody(io.NopCloser(bytes.NewReader(sent)))

			data, complete, err := body.Capture(context.Background())
			if err != nil {
				t.Fatalf("Capture() error = %v", err)
			}
			if complete != tc.complete {
				t.Fatalf("complete = %v, want %v", complete, tc.complete)
			}
			if want := min(tc.size, MaxCapturedBody); !bytes.Equal(data, sent[:want]) {
				t.Fatalf("captured %d bytes, want the first %d of what was sent", len(data), want)
			}
			got, err := io.ReadAll(body.Reader())
			if err != nil {
				t.Fatalf("reading the body on: %v", err)
			}
			if !bytes.Equal(got, sent) {
				t.Fatalf("sent on %d bytes, want the %d the sandbox sent", len(got), len(sent))
			}
		})
	}
}

// A body nobody asked to see is not read at all: most requests are decided
// without one, and reading it would hold them until it arrived.
func TestABodyNobodyCapturedIsTheOneThatArrived(t *testing.T) {
	source := io.NopCloser(strings.NewReader("untouched"))
	body := NewRequestBody(source)
	if got := body.Reader(); got != source {
		t.Fatalf("Reader() = %T, want the body the request arrived with", got)
	}
	if _, _, err := body.Capture(context.Background()); err == nil {
		t.Fatal("Capture() after the body was sent on succeeded, want it refused")
	}
}

// A request with no body has nothing to capture, and reads as empty.
func TestNoBodyIsAnEmptyOne(t *testing.T) {
	for _, source := range []io.ReadCloser{nil, http.NoBody} {
		body := NewRequestBody(source)
		if body != nil {
			t.Fatalf("NewRequestBody(%v) = %v, want nil", source, body)
		}
		data, complete, err := body.Capture(context.Background())
		if err != nil || !complete || len(data) != 0 {
			t.Fatalf("Capture() = %q, %v, %v, want empty and complete", data, complete, err)
		}
		if body.Reader() != http.NoBody {
			t.Fatal("Reader() of no body is not an empty one")
		}
	}
}

// Waiting for a body that has not arrived gives up when the context says so,
// and gives up nothing else: the read carries on, and what is sent on is still
// every byte, in order.
func TestAWaitThatGivesUpLosesNoBytes(t *testing.T) {
	reader, writer := io.Pipe()
	body := NewRequestBody(reader)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := body.Capture(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Capture() error = %v, want the wait to give up", err)
	}

	go func() {
		_, _ = writer.Write([]byte("late "))
		_, _ = writer.Write([]byte("but whole"))
		_ = writer.Close()
	}()
	got, err := io.ReadAll(body.Reader())
	if err != nil || string(got) != "late but whole" {
		t.Fatalf("sent on %q, %v, want every byte the sandbox sent", got, err)
	}
}

// A body that failed part way is sent on the same way: what was read, and
// then the failure, never a body that looks complete.
func TestABodyThatFailedFailsTheSameWay(t *testing.T) {
	broken := errors.New("the sandbox hung up")
	source := io.NopCloser(io.MultiReader(strings.NewReader("half"), failing{broken}))
	body := NewRequestBody(source)
	if _, _, err := body.Capture(context.Background()); !errors.Is(err, broken) {
		t.Fatalf("Capture() error = %v, want the read's own failure", err)
	}
	got, err := io.ReadAll(body.Reader())
	if !errors.Is(err, broken) || string(got) != "half" {
		t.Fatalf("sent on %q, %v, want what arrived and then the failure", got, err)
	}
}

type failing struct{ err error }

func (f failing) Read([]byte) (int, error) { return 0, f.err }
