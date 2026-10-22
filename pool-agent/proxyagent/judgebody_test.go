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

	"github.com/discobox-ai/discobox/pkg/judge"
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

// needsThenDecides is a judge that asks for the body on its first round and
// allows on the next.
func needsThenDecides() func(judgeAsk) response {
	return func(ask judgeAsk) response {
		if ask.Round == 1 {
			return response{status: http.StatusOK, body: map[string]any{
				"reason": "the operation is in the body", "need": map[string]any{"body": true},
			}}
		}
		return response{status: http.StatusOK, body: map[string]any{"allow": true, "reason": "that is the approved use"}}
	}
}

// A judge that asks to see the body is shown it, and its next answer is the
// verdict. The body it asked for is the request's own, as JSON, with nothing
// in it that stands for a credential.
func TestAJudgeThatAsksIsShownTheBody(t *testing.T) {
	resolver, asked := judgingPoolFunc(t, needsThenDecides())
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
	first := asks[0].Request.Body
	if first == nil || first.Supplied() || first.Length != int64(len(sent)) {
		t.Fatalf("first ask's body = %+v, want it described and not shown", first)
	}
	// Described by its parser from the first ask: what it is, not what it
	// carries.
	if first.Parser == nil || first.Parser.Name != judge.ParserJSON ||
		string(first.Metadata) != `{"keys":["title","token","head","draft","n"]}` {
		t.Fatalf("first ask's body = %+v (metadata %s), want its keys described by the JSON parser", first, first.Metadata)
	}
	shown := asks[1].Request.Body
	want := `{"title":"fix the tests","token":"<redacted>","head":"<redacted>","draft":false,"n":1.50}`
	if shown == nil || shown.Content == nil || *shown.Content != want || shown.Missing != "" {
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
	resolver, asked := judgingPoolFunc(t, needsThenDecides())
	if _, err := resolver.Authorize(context.Background(), withBody([]byte(`{"repo":"org/allowed","repo":"org/other"}`), "application/json")); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	if got := asked()[1].Request.Body.Content; got == nil || *got != `{"repo":"org/allowed","repo":"org/other"}` {
		t.Fatalf("content = %v, want both keys, in order", got)
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
		body    []byte
		header  map[string]string
		content string
		// unshown is a body none of which could be shown at all.
		unshown bool
		missing string
	}{
		{
			name:    "text past the budget",
			body:    bytes.Repeat([]byte("a"), judge.MaxBodyBytes+100),
			content: strings.Repeat("a", judge.MaxBodyBytes),
			missing: "only the first 8192 of its 8292 bytes are shown",
		},
		{
			name: "labeled JSON that is not", body: []byte("mutation{deleteRepo}"),
			header:  map[string]string{"Content-Type": "application/json"},
			missing: "the body stops being JSON after its first 0 bytes",
		},
		{
			name: "not text", body: []byte{0xff, 0xfe, 0x00, 0x01},
			missing: "the body is not text",
		},
		{
			name: "gzip, decoded", body: gzipped.Bytes(),
			header:  map[string]string{"Content-Encoding": "gzip"},
			content: `{"compressed":true}`,
		},
		{
			name: "an encoding this does not decode", body: []byte("\x1b\x00\x00"),
			header:  map[string]string{"Content-Encoding": "br"},
			unshown: true, missing: `the body is encoded as "br", which this proxy does not decode`,
		},
		{
			name:    "form-encoded, with a credential in it",
			body:    []byte("state=open&password=hunter2&base=main"),
			header:  map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
			content: "state=open&password=%3Credacted%3E&base=main",
		},
		{
			// The spellings a JSON body uses, which are not a query string's:
			// the shape of a secret created through the discobox API.
			name: "JSON, with camelCase credentials in it",
			body: []byte(`{"name":"gh","value":{"type":"oauth","token":"ghp_a","refreshToken":"ghr_b","clientSecret":"cs",` +
				`"tokenUrl":"https://example.com/token"},"headers":{"Authorization":"Bearer x"},"sortKey":"name"}`),
			header: map[string]string{"Content-Type": "application/json"},
			content: `{"name":"gh","value":{"type":"oauth","token":"<redacted>","refreshToken":"<redacted>","clientSecret":"<redacted>",` +
				`"tokenUrl":"https://example.com/token"},"headers":{"Authorization":"<redacted>"},"sortKey":"name"}`,
		},
		{
			// Its parser decides how a body is written, not the judge: JSON
			// is written back compact, losing its spacing and the same values.
			name:    "JSON, written back compact",
			body:    []byte(`{"title": "t", "accessToken": "ghp_a", "auth": {"password": "p", "list": [1, "two"]}}`),
			header:  map[string]string{"Content-Type": "application/json"},
			content: `{"title":"t","accessToken":"<redacted>","auth":{"password":"<redacted>","list":["<redacted>","<redacted>"]}}`,
		},
		{
			// Labeled as something else, it is text, and still redacted as
			// the JSON it is.
			name:    "JSON labeled as something else",
			body:    []byte(`[{"token": "ghp_a"}]`),
			content: `[{"token": "<redacted>"}]`,
		},
		{
			// Too large to parse as JSON, it is shown as sent: the part that
			// is shown is still redacted, and the value the read ended inside
			// is left out.
			name:    "JSON too large to parse",
			body:    append([]byte(`{"token":"ghp_a","a":"`), bytes.Repeat([]byte("b"), proxy.MaxCapturedSecretBody)...),
			header:  map[string]string{"Content-Type": "application/json"},
			content: `{"token":"<redacted>","a"`,
			missing: "the body is longer than this proxy reads",
		},
		{
			name:    "JSON that stops being JSON",
			body:    []byte(`{"a":1,"token": nope}`),
			header:  map[string]string{"Content-Type": "application/json"},
			content: `{"a":1,"token"`,
			missing: "the body stops being JSON after its first 14 bytes",
		},
		{
			// A form upload names its fields the way a form-encoded body
			// does, and a file in it does not make the rest unreadable.
			name: "multipart, with credentials and a file in it",
			body: []byte("--XB\r\nContent-Disposition: form-data; name=\"title\"\r\n\r\nfix it\r\n" +
				"--XB\r\nContent-Disposition: form-data; name=\"client_secret\"\r\n\r\ns3cr3t\r\n" +
				"--XB\r\nContent-Disposition: form-data; name=\"user[password]\"\r\n\r\nhunter2\r\n" +
				"--XB\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.bin\"\r\nContent-Type: application/octet-stream\r\n" +
				"X-Extra: ghp_itsown\r\n\r\n\xff\xfe\x00\r\n--XB--\r\n"),
			header: map[string]string{"Content-Type": "multipart/form-data; boundary=XB"},
			content: "--XB\r\nContent-Disposition: form-data; name=\"title\"\r\n\r\nfix it\r\n" +
				"--XB\r\nContent-Disposition: form-data; name=\"client_secret\"\r\n\r\n<redacted>\r\n" +
				"--XB\r\nContent-Disposition: form-data; name=\"user[password]\"\r\n\r\n<redacted>\r\n" +
				"--XB\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.bin\"\r\nContent-Type: application/octet-stream\r\n" +
				"X-Extra: <redacted>\r\n\r\n<3 bytes that are not text>\r\n--XB--\r\n",
		},
		{
			// Each part is redacted as what it says it is, and named by its
			// disposition whatever that is — not only form-data.
			name: "multipart/mixed, with JSON and form parts",
			body: []byte("--XB\r\nContent-Type: application/json\r\n\r\n{\"repo\": \"a/b\", \"access_token\": \"ghp_a\"}\r\n" +
				"--XB\r\nContent-Type: application/x-www-form-urlencoded\r\n\r\nstate=open&clientSecret=cs\r\n" +
				"--XB\r\nContent-Disposition: attachment; name=\"password\"\r\n\r\nhunter2\r\n" +
				"--XB\r\nContent-Type: application/json\r\n\r\n{\"a\":1,\"token\": nope}\r\n--XB--\r\n"),
			header: map[string]string{"Content-Type": "multipart/mixed; boundary=XB"},
			content: "--XB\r\nContent-Type: application/json\r\n\r\n{\"repo\": \"a/b\", \"access_token\": \"<redacted>\"}\r\n" +
				"--XB\r\nContent-Type: application/x-www-form-urlencoded\r\n\r\nstate=open&clientSecret=%3Credacted%3E\r\n" +
				"--XB\r\nContent-Disposition: attachment; name=\"password\"\r\n\r\n<redacted>\r\n" +
				"--XB\r\nContent-Type: application/json\r\n\r\n{\"a\":1,\"token\"\r\n--XB--\r\n", missing: "part 4 stops being JSON after its first 14 bytes",
		},
		{
			// A part is named by where it is, and multipart is shown two
			// bodies deep and no further: every level copies what is left,
			// and a body can nest as deep as its bytes allow.
			name: "multipart nested past the depth shown",
			body: []byte("--A\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n" +
				"--B\r\nContent-Type: application/json\r\n\r\n{\"x\": nope}\r\n" +
				"--B\r\nContent-Type: multipart/mixed; boundary=C\r\n\r\n--C\r\n\r\ndeep\r\n--C--\r\n\r\n--B--\r\n\r\n--A--\r\n"),
			header: map[string]string{"Content-Type": "multipart/mixed; boundary=A"},
			content: "--A\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n" +
				"--B\r\nContent-Type: application/json\r\n\r\n{\"x\"\r\n" +
				"--B\r\nContent-Type: multipart/mixed; boundary=C\r\n\r\n<20 bytes of multipart>\r\n--B--\r\n\r\n--A--\r\n",
			missing: "part 1.1 stops being JSON after its first 4 bytes, and nothing after that is shown; " +
				"part 1.2 is multipart inside multipart, which is described rather than shown",
		},
		{
			name: "multipart with no boundary", body: []byte("--XB\r\n"),
			header: map[string]string{"Content-Type": "multipart/form-data"}, missing: "names no boundary",
		},
		{
			name: "a sentinel as the encoding", body: []byte("x"),
			header:  map[string]string{"Content-Encoding": testEphemeral},
			unshown: true, missing: "which this proxy does not decode",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolver, asked := judgingPoolFunc(t, needsThenDecides())
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
			if tc.unshown != (shown.Content == nil) {
				t.Fatalf("shown = %+v, want content present = %v", shown, !tc.unshown)
			}
			content := ""
			if shown.Content != nil {
				content = *shown.Content
			}
			if content != tc.content || !strings.Contains(shown.Missing, tc.missing) || (tc.missing == "") != (shown.Missing == "") {
				t.Fatalf("shown = %+v (content %q)\nwant content %q, missing containing %q", shown, content, tc.content, tc.missing)
			}
			if strings.Contains(content+shown.Missing, testEphemeral) {
				t.Fatalf("shown = %+v, which carries a sentinel", shown)
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
		resolver, asked := judgingPool(t, map[string]any{"reason": "show me", "need": map[string]any{"body": true}}, http.StatusOK)
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
				"reason": "more", "need": map[string]any{"body": true, "bytes": 100 * ask.Round},
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
	resolver, asked := judgingPoolFunc(t, needsThenDecides())
	req := authorizeRequest()
	req.Header.Del("Content-Length")
	if _, err := resolver.Authorize(context.Background(), req); err != nil {
		t.Fatalf("Authorize() error = %v", err)
	}
	shown := asked()[1].Request.Body
	if shown == nil || shown.Content == nil || *shown.Content != "" || shown.Missing != "" || shown.Length != 0 {
		t.Fatalf("shown = %+v, want an empty body shown whole", shown)
	}
}
