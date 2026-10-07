package access

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/discobox-ai/discobox/agentcreds"
)

// debugFlag is the global flag, taken before the command, that prints every
// call this CLI makes to the credentials service.
const debugFlag = "--debug"

// redacted is what a credential's value is printed as.
const redacted = "<redacted>"

// debugTransport prints each call it carries to stderr: the method and URL,
// the JSON request body, the status, and the response body. It prints no
// header, so a bearer token never reaches it, and a credential value in a
// response is printed as redacted: printing one would be the unjudged way to
// take a value that ADR 0092 says this CLI does not have.
type debugTransport struct {
	next http.RoundTripper
}

func (d debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// os.Stderr is read on every call rather than kept, so what reads the
	// process's stderr — a test included — sees the lines.
	out := os.Stderr
	fmt.Fprintf(out, "%s: debug: %s %s\n", Name, req.Method, req.URL.Redacted())
	if req.Body != nil && req.Body != http.NoBody {
		body, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		fmt.Fprintf(out, "%s: debug: request body: %s\n", Name, body)
	}
	resp, err := d.next.RoundTrip(req)
	if err != nil {
		fmt.Fprintf(out, "%s: debug: no response: %v\n", Name, err)
		return nil, err
	}
	fmt.Fprintf(out, "%s: debug: %s\n", Name, resp.Status)
	// Read as far as the client would, and hand it the same stream: what was
	// read, then whatever is left.
	body, err := io.ReadAll(io.LimitReader(resp.Body, agentcreds.MaxBodyBytes+1))
	if err != nil {
		_ = resp.Body.Close()
		fmt.Fprintf(out, "%s: debug: response body unreadable: %v\n", Name, err)
		return nil, err
	}
	resp.Body = readCloser{io.MultiReader(bytes.NewReader(body), resp.Body), resp.Body}
	if len(body) > 0 {
		fmt.Fprintf(out, "%s: debug: response body: %s\n", Name, redactValue(req.URL.Path, body))
	}
	return resp, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// redactValue is a response body with any credential value in it replaced.
// The protocol carries a value in one place, the top-level "value" of a use
// response, and that key is redacted in any response that has it. A use
// response that cannot be read as a JSON object is not shown at all: what it
// holds cannot be told apart from a value.
func redactValue(path string, body []byte) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		if strings.HasSuffix(path, agentcreds.PathUse) {
			return redacted
		}
		return string(bytes.TrimSpace(body))
	}
	value, ok := fields["value"]
	if !ok {
		return string(bytes.TrimSpace(body))
	}
	if string(value) != `""` && string(value) != "null" {
		fields["value"] = json.RawMessage(`"` + redacted + `"`)
	}
	var shown bytes.Buffer
	encoder := json.NewEncoder(&shown)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(fields); err != nil {
		return redacted
	}
	return strings.TrimSpace(shown.String())
}
