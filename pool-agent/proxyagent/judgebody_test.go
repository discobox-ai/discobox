package proxyagent

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/proxy"
)

// withBody is authorizeRequest carrying body, declared the way a client that
// measured it would.
func withBody(body []byte, contentType string) proxy.SecretAuthorizeRequest {
	req := authorizeRequest()
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	req.Body = proxy.NewSecretRequestBody(io.NopCloser(bytes.NewReader(body)))
	return req
}

// needsThenDecides is a judge that asks for the body in form on its first
// round and allows on the next.
func needsThenDecides(form string) func(judgeAsk) response {
	return func(ask judgeAsk) response {
		if ask.Round == 1 {
			return response{status: http.StatusOK, body: map[string]any{
				"reason": "the operation is in the body", "need": map[string]any{"body": form},
			}}
		}
		return response{status: http.StatusOK, body: map[string]any{"allow": true, "reason": "that is the approved use"}}
	}
}

// A judge that asks to see the body is shown it, and its next answer is the
// verdict. The body it asked for is the request's own, as JSON, with nothing
// in it that stands for a credential.
func TestAJudgeThatAsksIsShownTheBody(t *testing.T) {
	resolver, asked := judgingPoolFunc(t, needsThenDecides(judge.FormJSON))
	sent := []byte(`{"title": "fix the tests", "token": "ghp_itsownsecret", "head": "` + testEphemeral + `", "draft": false, "n": 1.50}`)
	req := withBody(sent, "application/json")

	verdict, err := resolver.Authorize(context.Background(), req)
	if err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if !verdict.Allow {
		t.Fatalf("verdict = %+v, want the second round's allow", verdict)
	}
	asks := asked()
	if len(asks) != 2 || asks[0].Round != 1 || asks[1].Round != 2 {
		t.Fatalf("asks = %+v, want a first round and a second", asks)
	}
	if first := asks[0].Request.Body; first == nil || first.Supplied() || first.Length != int64(len(sent)) {
		t.Fatalf("first ask's body = %+v, want it described and not shown", first)
	}
	shown := asks[1].Request.Body
	want := `{"title":"fix the tests","token":"<redacted>","head":"<redacted>","draft":false,"n":1.50}`
	if shown == nil || shown.Form != judge.FormJSON || shown.Content != want || shown.Missing != "" {
		t.Fatalf("second ask's body = %+v, want the whole body as JSON, redacted:\n%s", shown, want)
	}
	// And the proxy still has every byte of it to send.
	got, err := io.ReadAll(req.Body.Reader())
	if err != nil || !bytes.Equal(got, sent) {
		t.Fatalf("the body sent on is %q, %v; want what the sandbox sent", got, err)
	}
}

// JSON is written back from what was sent, not from a map: a key said twice
// is shown twice, since the upstream may read the one a map would have kept
// out of sight.
func TestAKeySaidTwiceIsShownTwice(t *testing.T) {
	resolver, asked := judgingPoolFunc(t, needsThenDecides(judge.FormJSON))
	if _, err := resolver.Authorize(context.Background(), withBody([]byte(`{"repo":"org/allowed","repo":"org/other"}`), "application/json")); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if got := asked()[1].Request.Body.Content; got != `{"repo":"org/allowed","repo":"org/other"}` {
		t.Fatalf("content = %s, want both keys, in order", got)
	}
}

// What cannot be shown is said, and the judge decides knowing it.
func TestWhatCannotBeShownIsSaid(t *testing.T) {
	var gzipped bytes.Buffer
	writer := gzip.NewWriter(&gzipped)
	_, _ = writer.Write([]byte(`{"compressed":true}`))
	_ = writer.Close()

	for _, tc := range []struct {
		name    string
		form    string
		body    []byte
		header  map[string]string
		content string
		shownAs string
		missing string
	}{
		{
			name: "text past the budget", form: judge.FormText,
			body:    bytes.Repeat([]byte("a"), judge.MaxBodyBytes+100),
			content: strings.Repeat("a", judge.MaxBodyBytes), shownAs: judge.FormText,
			missing: "only the first 8192 of its 8292 bytes are shown",
		},
		{
			name: "JSON too large to parse", form: judge.FormJSON,
			body:    append([]byte(`{"a":"`), bytes.Repeat([]byte("b"), proxy.MaxCapturedSecretBody)...),
			shownAs: judge.FormJSON, missing: "cannot be parsed as JSON; it can be shown as text",
		},
		{
			name: "not JSON", form: judge.FormJSON, body: []byte("mutation{deleteRepo}"),
			shownAs: judge.FormJSON, missing: "the body is not JSON",
		},
		{
			name: "not text", form: judge.FormText, body: []byte{0xff, 0xfe, 0x00, 0x01},
			shownAs: judge.FormText, missing: "the body is not text",
		},
		{
			name: "gzip, decoded", form: judge.FormJSON, body: gzipped.Bytes(),
			header:  map[string]string{"Content-Encoding": "gzip"},
			content: `{"compressed":true}`, shownAs: judge.FormJSON,
		},
		{
			name: "an encoding this does not decode", form: judge.FormText, body: []byte("\x1b\x00\x00"),
			header:  map[string]string{"Content-Encoding": "br"},
			missing: `the body is encoded as "br", which this proxy does not decode`,
		},
		{
			name: "form-encoded, with a credential in it", form: judge.FormText,
			body:    []byte("state=open&password=hunter2&base=main"),
			header:  map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
			content: "state=open&password=%3Credacted%3E&base=main", shownAs: judge.FormText,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver, asked := judgingPoolFunc(t, needsThenDecides(tc.form))
			req := withBody(tc.body, "text/plain")
			for name, value := range tc.header {
				req.Header.Set(name, value)
			}
			if _, err := resolver.Authorize(context.Background(), req); err != nil {
				t.Fatalf("Authorize() error = %v", err)
			}
			asks := asked()
			if len(asks) != 2 {
				t.Fatalf("asked %d times, want the body shown on a second round", len(asks))
			}
			shown := asks[1].Request.Body
			if shown.Form != tc.shownAs || shown.Content != tc.content || !strings.Contains(shown.Missing, tc.missing) ||
				(tc.missing == "") != (shown.Missing == "") {
				t.Fatalf("shown = %+v\nwant form %q, content %q, missing containing %q", shown, tc.shownAs, tc.content, tc.missing)
			}
			// What the control plane will check, checked here: a later round
			// the judge package would refuse is a body nobody sees.
			if err := (judge.Job{Kind: judge.KindRequest, Purpose: "p", Host: "h", Round: 2, Request: asks[1].Request}).Validate(); err != nil {
				t.Fatalf("the second round is not a job the control plane would take: %v", err)
			}
		})
	}
}

// Asking is not allowing. A judge that asks again for what it has been shown
// has decided nothing, and neither has one still asking on its last round.
func TestAJudgeThatNeverDecidesHasNotAllowed(t *testing.T) {
	t.Run("asking again for the same thing", func(t *testing.T) {
		resolver, asked := judgingPool(t, map[string]any{"reason": "show me", "need": map[string]any{"body": "json"}}, http.StatusOK)
		verdict, err := resolver.Authorize(context.Background(), withBody([]byte(`{"a":1}`), "application/json"))
		if err != nil {
			t.Fatalf("Authorize() error = %v", err)
		}
		if verdict.Allow || !strings.Contains(verdict.Reason, "already been shown") {
			t.Fatalf("verdict = %+v, want a refusal for making no progress", verdict)
		}
		if len(asked()) != 2 {
			t.Fatalf("asked %d times, want the ask that showed it and no more", len(asked()))
		}
	})
	t.Run("still asking on the last round", func(t *testing.T) {
		// Each round asks for more than the last showed, so each is progress,
		// and the rounds run out anyway.
		resolver, asked := judgingPoolFunc(t, func(ask judgeAsk) response {
			return response{status: http.StatusOK, body: map[string]any{
				"reason": "more", "need": map[string]any{"body": "text", "bytes": 100 * ask.Round},
			}}
		})
		verdict, err := resolver.Authorize(context.Background(), withBody(bytes.Repeat([]byte("x"), 1000), "text/plain"))
		if err != nil {
			t.Fatalf("Authorize() error = %v", err)
		}
		if verdict.Allow || !strings.Contains(verdict.Reason, "last round") {
			t.Fatalf("verdict = %+v, want a refusal once the rounds ran out", verdict)
		}
		if len(asked()) != judge.MaxRounds {
			t.Fatalf("asked %d times, want %d", len(asked()), judge.MaxRounds)
		}
	})
}

// A request with no body that the judge asks to see is shown as one: whole,
// and empty.
func TestNoBodyIsShownAsEmpty(t *testing.T) {
	resolver, asked := judgingPoolFunc(t, needsThenDecides(judge.FormJSON))
	req := authorizeRequest()
	req.Header.Del("Content-Length")
	if _, err := resolver.Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	shown := asked()[1].Request.Body
	if shown == nil || shown.Form != judge.FormJSON || shown.Content != "" || shown.Missing != "" || shown.Length != 0 {
		t.Fatalf("shown = %+v, want an empty body shown whole", shown)
	}
}
