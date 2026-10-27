package access

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
)

// debugFlag is the global flag, taken before the command, that prints every
// call this CLI makes to the credentials service.
const debugFlag = "--debug"

// debugTransport prints each call it carries to stderr: the method and URL,
// the JSON request body, the status, and the response body. It prints no
// header, so a bearer token never reaches it. No call of the protocol carries
// a credential's value, so a body is printed as it is.
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
		fmt.Fprintf(out, "%s: debug: response body: %s\n", Name, bytes.TrimSpace(body))
	}
	return resp, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}
