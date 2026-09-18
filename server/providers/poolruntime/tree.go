package poolruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// A sandbox's durable tree moves as an opaque tar between the control plane and
// a pool agent (ADR 0123). The generated pool-agent client is not used for
// either direction: the body has no schema, it is unbounded, and both ends
// stream it, none of which the contract can express.

// ExportTree streams the sandbox's durable tree out of the pool hosting it.
//
// Runtime state names the pool when there is any, because that is where a
// sandbox that has actually run lives. When there is none -- a create that
// failed before the agent reported, which is a sandbox worth exporting rather
// than one to refuse -- the pool the caller names is used instead.
func (p *Provider) ExportTree(ctx context.Context, ref sandbox.SandboxRef, poolID string, image sandbox.ImageRef, state []byte) (io.ReadCloser, error) {
	client, err := p.agentClientFromState(ctx, ref, state)
	if err != nil {
		if !errors.Is(err, sandbox.ErrNotFound) || strings.TrimSpace(poolID) == "" {
			return nil, err
		}
		// Guarded here as ImportTree guards it: with empty state this is the
		// first touch of the manager, where every other path has already
		// dereferenced it.
		if p.manager == nil {
			return nil, fmt.Errorf("pool manager is required")
		}
		pool, poolErr := p.manager.GetPool(ctx, ref.ProjectID, poolID)
		if poolErr != nil {
			return nil, poolErr
		}
		if client, err = p.agentClientForPool(ctx, pool); err != nil {
			return nil, err
		}
	}
	return client.ExportTree(ctx, ref, image)
}

// ImportTree restores a durable tree onto the pool the sandbox is destined for.
//
// The pool is resolved the way a create resolves one -- the named pool if there
// is one, else the project's schedulable pool -- and waited for on the same
// budget, because a restore is the first thing that touches the host and it can
// arrive while the pool is still coming up. The pool it settles on is returned
// so the row created afterwards names the pool the data is actually on.
func (p *Provider) ImportTree(ctx context.Context, ref sandbox.SandboxRef, poolID string, tree io.Reader) (string, error) {
	if p.manager == nil {
		return "", fmt.Errorf("pool manager is required")
	}
	pool, err := p.schedulablePool(ctx, &model.Sandbox{
		ID:        ref.SandboxID,
		ProjectID: ref.ProjectID,
		PoolID:    poolID,
	})
	if err != nil {
		return "", err
	}
	lease, err := p.awaitPoolAgentClient(ctx, pool)
	if err != nil {
		return "", err
	}
	client := &poolAgentClient{poolID: pool.ID, tokenIssuer: p.manager, lease: lease}
	if err := client.ImportTree(ctx, ref, tree); err != nil {
		return "", err
	}
	return pool.ID, nil
}

func (p *poolAgentClient) ExportTree(ctx context.Context, ref sandbox.SandboxRef, image sandbox.ImageRef) (io.ReadCloser, error) {
	lease, err := p.treeLease(ref, poolagentauth.ScopeSandboxRead)
	if err != nil {
		return nil, err
	}
	// The image the pool reads the tree with (ADR 0129 §1). The parameter names
	// mirror the pool agent's own (TreeImageParam, TreeImageDigestParam).
	query := url.Values{}
	if image.Name != "" {
		query.Set("image", image.Name)
	}
	if image.Digest != "" {
		query.Set("imageDigest", image.Digest)
	}
	resp, err := p.treeRequest(ctx, http.MethodGet, ref, lease, query, nil)
	if err != nil {
		lease.Release()
		return nil, poolAgentTransportError("read the sandbox tree from", p.poolID, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		lease.Release()
		return nil, treeStatusError(resp)
	}
	// The lease holds the transport this body is being read over, so it is
	// released when the body is closed and not when this returns.
	return &leasedReader{ReadCloser: resp.Body, lease: lease}, nil
}

func (p *poolAgentClient) ImportTree(ctx context.Context, ref sandbox.SandboxRef, tree io.Reader) error {
	lease, err := p.treeLease(ref, poolagentauth.ScopeSandboxWrite)
	if err != nil {
		return err
	}
	defer lease.Release()
	resp, err := p.treeRequest(ctx, http.MethodPut, ref, lease, nil, tree)
	if err != nil {
		return poolAgentTransportError("send the sandbox tree to", p.poolID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return treeStatusError(resp)
	}
	return nil
}

// treeLease spends this client's single-use lease, the way poolClient does for
// a generated call, and attaches the token provider the request authenticates
// with.
func (p *poolAgentClient) treeLease(ref sandbox.SandboxRef, scope string) (*transport.HTTPClientLease, error) {
	lease := p.lease
	p.lease = nil
	if lease == nil {
		return nil, fmt.Errorf("pool-agent client for pool %s is spent; acquire one per call", p.poolID)
	}
	if lease.AuthTokenProvider == nil {
		lease.AuthTokenProvider = p.authTokenProvider(ref, scope)
	}
	return lease, nil
}

func (p *poolAgentClient) treeRequest(ctx context.Context, method string, ref sandbox.SandboxRef, lease *transport.HTTPClientLease, query url.Values, body io.Reader) (*http.Response, error) {
	baseURL := defaultPoolBaseURL
	if strings.TrimSpace(lease.BaseURL) != "" {
		baseURL = strings.TrimRight(lease.BaseURL, "/")
	}
	target := fmt.Sprintf("%s/api/project/%s/pool/%s/sandboxes/%s/tree",
		baseURL, url.PathEscape(ref.ProjectID), url.PathEscape(p.poolID), url.PathEscape(ref.SandboxID))
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", treeMediaType)
		// The length is not knowable: the tar is being produced as it is sent.
		// Saying so explicitly keeps net/http from buffering the whole tree to
		// find out.
		req.ContentLength = -1
	}
	if lease.AuthTokenProvider != nil {
		token, err := lease.AuthTokenProvider(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := http.DefaultClient
	if lease.Client != nil {
		client = lease.Client
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// treeMediaType mirrors the pool agent's own; it is stated here rather than
// imported so the server module does not reach into the agent's HTTP package
// for a constant.
const treeMediaType = "application/x-tar"

// poolAgentTransportError reports a failed hop to the pool agent without the
// pool agent's address in it.
//
// http.Client wraps every transport failure in a *url.Error carrying the whole
// target, and this target is one the caller cannot see and did not ask about:
// the agent's internal host and port, the project, the pool, and the sandbox id
// an import had just allocated. Returned as it comes, an archive that ended
// early reaches the user as
//
//	Put "http://127.0.0.1:32791/api/project/proj_.../pool/pool_.../sandboxes/sbx_.../tree": unexpected EOF
//
// which describes a machine they have no access to and buries the one word that
// tells them what went wrong. The cause is kept and the address is dropped.
func poolAgentTransportError(operation, poolID string, err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	return fmt.Errorf("%s pool %s: %w", operation, poolID, err)
}

// treeStatusError turns the agent's refusal into an error the control plane can
// answer with.
//
// "It is running" and "this pool already holds that sandbox" are answers a
// person acts on, so they have to survive the hop as statuses rather than
// collapse into a 500 about a pool the caller never asked about. A status error
// is what the HTTP layer already reads (statusCodeForProxyError), so carrying
// the agent's own code is all it takes.
func treeStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxUnexpectedBody))
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotFound {
		// Also matched as sandbox.ErrNotFound, because a missing tree is a
		// missing sandbox to everything upstream of here.
		return fmt.Errorf("%w: %s", sandbox.ErrNotFound, message)
	}
	return apperrors.NewStatusError(resp.StatusCode, message)
}

// leasedReader keeps a pool-agent transport lease alive for as long as the body
// read over it is open.
type leasedReader struct {
	io.ReadCloser
	lease *transport.HTTPClientLease
}

func (l *leasedReader) Close() error {
	err := l.ReadCloser.Close()
	if l.lease != nil {
		l.lease.Release()
		l.lease = nil
	}
	return err
}
