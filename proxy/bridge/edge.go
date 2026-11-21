package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// EdgeFile is the edge file's name in the proxy material directory, which the
// runtime-config intake writes from the document's proxy.edge.
const EdgeFile = "edge.json"

// Edge is how a sandbox reaches its pool's edge, as the intake writes it from
// a runtime-config document (sandboxconfig.RuntimeEdge) and every tunnel
// reads it, afresh at each tunnel it opens: the address to dial in place of
// the edge's name, and the credential the edge admits the pool's listener
// with (ADR 26-11-16-619 §§2–3). So a renewed credential or a moved address
// is taken without the bridge being restarted, and a tunnel already open is
// left alone.
type Edge struct {
	// Address is dialed in place of the edge's name, keeping the URL's port,
	// and nothing is resolved. Empty resolves the name through the
	// provider's resolver, as without an edge file.
	Address string `json:"address,omitempty"`
	// Header and Token are the credential, sent on every tunnel's opening
	// request. Empty sends none.
	Header string `json:"header,omitempty"`
	Token  string `json:"token,omitempty"`
}

// LoadEdge reads the edge file at path.
func LoadEdge(path string) (Edge, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Edge{}, fmt.Errorf("the pool's edge: %w", err)
	}
	var edge Edge
	if err := json.Unmarshal(data, &edge); err != nil {
		return Edge{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return edge, nil
}

// edgeHTTPClient carries a wss tunnel's opening request to the edge: over
// HTTP/1.1 alone, since a WebSocket opens with an HTTP/1.1 Upgrade and an
// edge's port credential is checked on HTTP/1.1 (ADR 26-11-16-619 §2), with
// the credential the edge file at edgePath names, dialed at its address. With
// no edge file named it resolves the edge's name through the provider's
// resolver (ADR 26-11-12-355 §3) and sends no credential.
func edgeHTTPClient(edgePath string) *http.Client {
	resolve := providerDialContext(ProviderResolverPath)
	transport := &http.Transport{
		DialContext:         resolve,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	transport.Protocols = &protocols
	if edgePath == "" {
		return &http.Client{Transport: transport}
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		edge, err := LoadEdge(edgePath)
		if err != nil {
			return nil, err
		}
		if edge.Address == "" {
			return resolve(ctx, network, address)
		}
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		// The TLS server name and the Host stay the edge's name, which the
		// transport takes from the URL: the edge routes by name, so a stale
		// address still reaches the pool.
		direct := net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		return direct.DialContext(ctx, network, net.JoinHostPort(edge.Address, port))
	}
	return &http.Client{Transport: edgeCredential{path: edgePath, next: transport}}
}

// edgeCredential sends the edge file's credential on each request, read as
// the file is then.
type edgeCredential struct {
	path string
	next http.RoundTripper
}

func (c edgeCredential) RoundTrip(req *http.Request) (*http.Response, error) {
	edge, err := LoadEdge(c.path)
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	if edge.Header != "" || edge.Token != "" {
		// The intake writes the file from a document it validated
		// (sandboxconfig.ValidateEdgeCredential); half a credential is a file
		// it did not write.
		if edge.Header == "" || edge.Token == "" {
			if req.Body != nil {
				_ = req.Body.Close()
			}
			return nil, errors.New("the pool's edge file names half a credential")
		}
		req = req.Clone(req.Context())
		req.Header.Set(edge.Header, edge.Token)
	}
	return c.next.RoundTrip(req)
}
