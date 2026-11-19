package secrets

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMatchMessageFindsOnlyWholeStrings(t *testing.T) {
	sw := bodySwapper("v", nil)
	encoded := base64.StdEncoding.EncodeToString([]byte("user:" + bodySentinel))
	for _, tc := range []struct {
		name     string
		message  string
		text     bool
		retained []string
		want     string // "at" of each use, space separated
		refused  string
	}{
		{name: "identify", message: `{"op":2,"d":{"token":"bxd_SENTINEL"}}`, text: true, want: "/d/token"},
		{name: "every whole string, wherever", message: `{"op":2,"d":{"token":"bxd_SENTINEL","x/y":["bxd_SENTINEL"]}}`, text: true, want: "/d/token /d/x~1y/0"},
		{name: "escaped is still whole", message: `{"d":{"token":"\u0062xd_SENTINEL"}}`, text: true, want: "/d/token"},
		{name: "no sentinel", message: `{"op":1,"d":42}`, text: true},
		{name: "no sentinel, not JSON", message: `hello`, text: true},
		{name: "no sentinel, binary", message: "\x00\x01", text: false},
		{name: "inside a longer string", message: `{"d":{"name":"I am bxd_SENTINEL"}}`, text: true, refused: "inside the string at /d/name"},
		{name: "escaped inside a longer string", message: `{"d":{"name":"x\u0062xd_SENTINEL"}}`, text: true, refused: "inside the string"},
		{name: "a member name", message: `{"bxd_SENTINEL":1}`, text: true, refused: "member name under the top level"},
		{name: "a nested member name", message: `{"d":{"\u0062xd_SENTINEL":{"t":"bxd_SENTINEL"}}}`, text: true, refused: "member name under /d"},
		{name: "base64", message: `{"d":{"auth":"` + encoded + `"}}`, text: true, refused: "inside the string"},
		{name: "binary", message: "\x00bxd_SENTINEL", text: false, refused: "binary message"},
		{name: "not one object", message: `["bxd_SENTINEL"]`, text: true, refused: "not a JSON object"},
		{name: "a member twice", message: `{"t":"bxd_SENTINEL","t":"x"}`, text: true, refused: "not a JSON object"},
		{name: "retained", message: `{"t":"bxd_GONE"}`, text: true, retained: []string{"bxd_GONE"}, refused: "no longer holds"},
		{name: "retained inside", message: `{"t":"a bxd_GONE"}`, text: true, retained: []string{"bxd_GONE"}, refused: "inside the string"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uses, err := sw.MatchMessage("sandbox-1", []byte(tc.message), tc.text, tc.retained)
			var refusal *MessageRefusal
			if tc.refused != "" {
				if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, tc.refused) {
					t.Fatalf("MatchMessage error = %v, want a refusal %q", err, tc.refused)
				}
				if strings.Contains(refusal.Reason, bodySentinel) {
					t.Fatalf("a refusal names the sentinel: %s", refusal.Reason)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, place := range uses.Places() {
				got = append(got, place.At)
			}
			if strings.Join(got, " ") != tc.want {
				t.Fatalf("uses = %v, want %q", got, tc.want)
			}
			if tc.want != "" && uses.Message() == nil {
				t.Fatal("no message for the authorizer")
			}
		})
	}
}

func TestApplyMessageSwapsAtEachPlace(t *testing.T) {
	sw := bodySwapper("real\"value</>", nil)
	uses, err := sw.MatchMessage("sandbox-1", []byte(`{"op":2, "d":{"token":"bxd_SENTINEL"} ,"x":"bxd_SENTINEL"}`), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, res, echoes := sw.ApplyMessage(context.Background(), "sandbox-1", "gateway.discord.gg:443", "/", uses, time.Now().Add(time.Second))
	if want := `{"op":2, "d":{"token":"real\"value</>"} ,"x":"real\"value</>"}`; string(out) != want {
		t.Fatalf("message = %s\nwant      %s", out, want)
	}
	if len(res.Errors) > 0 || len(res.BodyPlaces) != 2 {
		t.Fatalf("result %+v", res)
	}
	// What the capture keeps out of the origin's answers: the value, and it
	// as the inside of a JSON string.
	var forms []string
	for _, echo := range echoes {
		forms = append(forms, string(echo))
	}
	for _, want := range []string{`real"value</>`, `real\"value</>`} {
		if !slices.Contains(forms, want) {
			t.Fatalf("echoes %q lack %q", forms, want)
		}
	}

	denied := bodySwapper("", ErrDenied)
	uses, _ = denied.MatchMessage("sandbox-1", []byte(`{"t":"bxd_SENTINEL"}`), true, nil)
	if out, res, _ := denied.ApplyMessage(context.Background(), "sandbox-1", "h", "/", uses, time.Now().Add(time.Second)); out != nil || len(res.Errors) > 0 {
		t.Fatalf("a denial changed the message: %s %+v", out, res)
	}

	down := bodySwapper("", errors.New("control plane unreachable"))
	uses, _ = down.MatchMessage("sandbox-1", []byte(`{"t":"bxd_SENTINEL"}`), true, nil)
	if out, res, _ := down.ApplyMessage(context.Background(), "sandbox-1", "h", "/", uses, time.Now().Add(50*time.Millisecond)); out != nil || len(res.Errors) == 0 {
		t.Fatalf("a transient failure sent: %s %+v", out, res)
	}
}

// A message resolved under one configuration is sent only under the same
// one, and only while its sentinels are still the client's.
func TestDecideRefusesAStaleRevision(t *testing.T) {
	sw := bodySwapper("v", nil)
	sent := 0
	send := func(context.Context) error { sent++; return nil }
	revision := sw.Revision()
	if stale, err := sw.Decide(context.Background(), revision, "sandbox-1", []string{bodySentinel}, send); stale || err != nil || sent != 1 {
		t.Fatalf("stale=%v err=%v sent=%d", stale, err, sent)
	}
	sw.Reconfigure(Config{Sentinels: map[string][]string{"sandbox-1": {bodySentinel}}})
	if stale, _ := sw.Decide(context.Background(), revision, "sandbox-1", []string{bodySentinel}, send); !stale || sent != 1 {
		t.Fatalf("an old revision sent: stale=%v sent=%d", stale, sent)
	}
	revision = sw.Revision()
	if stale, _ := sw.Decide(context.Background(), revision, "sandbox-1", []string{"bxd_OTHER"}, send); !stale || sent != 1 {
		t.Fatalf("a sentinel the client does not hold sent: stale=%v sent=%d", stale, sent)
	}
	// The decision is bounded: one that waits on its context ends by
	// decideBound, and so does the publish lock it holds.
	start := time.Now()
	if _, err := sw.Decide(context.Background(), revision, "sandbox-1", []string{bodySentinel}, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}); err == nil || time.Since(start) > 2*decideBound {
		t.Fatalf("a waiting decision returned %v after %v", err, time.Since(start))
	}
	applied := make(chan struct{})
	go func() {
		sw.Reconfigure(Config{Sentinels: map[string][]string{"sandbox-1": {bodySentinel}}})
		close(applied)
	}()
	select {
	case <-applied:
	case <-time.After(2 * decideBound):
		t.Fatal("a configuration waited past the decision's bound")
	}
}
