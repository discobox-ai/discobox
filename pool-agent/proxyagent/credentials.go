package proxyagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
)

// The pool half of the agent credentials protocol (ADR 0031).
//
// It lives in the proxy unit rather than the pool-agent process on purpose:
// minting an ephemeral sentinel, registering it with the proxy's match set, and
// translating it back at swap time are one indivisible act, and splitting them
// across two processes would put a file and a race between the moment a sandbox
// is handed a sentinel and the moment the proxy would recognize it.
//
// The sandbox reaches it over the same mTLS material the egress bridge already
// uses, so the client certificate — issued per sandbox, CN = sandbox ID — is
// the identity. The sandbox holds no control-plane credential and takes part in
// no authorization decision.

// credentialBrokerTimeout bounds one control-plane call made on a sandbox's
// behalf. It is shorter than a human's patience and longer than any healthy
// round trip; a hung control plane must fail the agent's call, not hold its
// process open.
const credentialBrokerTimeout = 15 * time.Second

// controlPlaneCredentials calls the control plane's agent credentials broker
// routes with the scoped token from ResolveContextFile — the same file the
// sentinel resolver reads, re-read per call so a token refresh takes effect
// without restarting the unit.
type controlPlaneCredentials struct {
	contextPath string
	client      *http.Client
}

type credentialUseDoc struct {
	UseID       string `json:"useId,omitempty"`
	Description string `json:"description"`
}

type credentialDoc struct {
	Name      string             `json:"name"`
	EnvVar    string             `json:"envVar"`
	Host      string             `json:"host"`
	SecretID  string             `json:"secretId"`
	GrantID   string             `json:"grantId"`
	Sentinel  string             `json:"sentinel"`
	Format    string             `json:"format,omitempty"`
	ExpiresAt *time.Time         `json:"expiresAt,omitempty"`
	Uses      []credentialUseDoc `json:"uses,omitempty"`
}

type listCredentialsDoc struct {
	Credentials []credentialDoc `json:"credentials"`
}

type createCredentialRequestDoc struct {
	SandboxID     string             `json:"sandboxId"`
	Name          string             `json:"name"`
	EnvVar        string             `json:"envVar"`
	Host          string             `json:"host"`
	Justification string             `json:"justification,omitempty"`
	Uses          []credentialUseDoc `json:"uses"`
}

type credentialRequestStatusDoc struct {
	RequestID string             `json:"requestId"`
	Status    string             `json:"status"`
	Uses      []credentialUseDoc `json:"uses,omitempty"`
}

func (c *controlPlaneCredentials) list(ctx context.Context, sandboxID string) ([]credentialDoc, error) {
	var out listCredentialsDoc
	query := url.Values{"sandboxId": []string{sandboxID}}
	if err := c.do(ctx, http.MethodGet, "sandbox-credentials", query, nil, &out); err != nil {
		return nil, err
	}
	return out.Credentials, nil
}

func (c *controlPlaneCredentials) createRequest(ctx context.Context, body createCredentialRequestDoc) (credentialRequestStatusDoc, error) {
	var out credentialRequestStatusDoc
	err := c.do(ctx, http.MethodPost, "sandbox-credential-requests", nil, body, &out)
	return out, err
}

func (c *controlPlaneCredentials) requestStatus(ctx context.Context, sandboxID, requestID string) (credentialRequestStatusDoc, error) {
	var out credentialRequestStatusDoc
	query := url.Values{"sandboxId": []string{sandboxID}}
	path := "sandbox-credential-requests/" + url.PathEscape(requestID)
	err := c.do(ctx, http.MethodGet, path, query, nil, &out)
	return out, err
}

func (c *controlPlaneCredentials) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	rc, err := readResolveContext(c.contextPath)
	if err != nil || rc.Token == "" || rc.ControlPlaneURL == "" || rc.PoolID == "" {
		// No usable credential yet: fail closed, the same way the resolver does
		// when it cannot prove who it is.
		return fmt.Errorf("%w: pool has no control-plane credential yet", agentcreds.ErrDenied)
	}
	endpoint := fmt.Sprintf("%s/api/pools/%s/%s", rc.ControlPlaneURL, url.PathEscape(rc.PoolID), path)
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+rc.Token)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return controlPlaneError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// controlPlaneError maps a control-plane failure onto the protocol's errors, so
// a refusal reaches the agent as a refusal rather than as a generic 500 from
// two hops away.
func controlPlaneError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	message := strings.TrimSpace(string(data))
	// The control plane answers errors as RFC 7807 problem documents; surface
	// the human-readable half and drop the envelope.
	var problem struct {
		Detail string `json:"detail"`
		Title  string `json:"title"`
	}
	if json.Unmarshal(data, &problem) == nil {
		if detail := strings.TrimSpace(problem.Detail); detail != "" {
			message = detail
		} else if title := strings.TrimSpace(problem.Title); title != "" {
			message = title
		}
	}
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", agentcreds.ErrNotFound, message)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %s", agentcreds.ErrDenied, message)
	case http.StatusBadRequest, http.StatusConflict:
		return fmt.Errorf("%w: %s", agentcreds.ErrInvalid, message)
	default:
		return fmt.Errorf("control plane returned %d: %s", resp.StatusCode, message)
	}
}

// credentialBroker serves the protocol for one sandbox. A fresh one is built
// per connection from the client certificate, so a sandbox's identity is
// structurally not something it can pass in a request body.
type credentialBroker struct {
	sandboxID   string
	controlPlan *controlPlaneCredentials
	activations *activations
}

var _ agentcreds.Service = (*credentialBroker)(nil)

func (b *credentialBroker) List(ctx context.Context) ([]agentcreds.Credential, error) {
	docs, err := b.controlPlan.list(ctx, b.sandboxID)
	if err != nil {
		return nil, err
	}
	out := make([]agentcreds.Credential, 0, len(docs))
	for _, doc := range docs {
		// Sentinel and format stay here. list is what the sandbox sees, and the
		// stable sentinel is the one thing on the trusted side that would let a
		// sandbox address the credential directly.
		out = append(out, agentcreds.Credential{
			Name:   doc.Name,
			EnvVar: doc.EnvVar,
			Host:   doc.Host,
			Uses:   protocolUses(doc.Uses, doc.ExpiresAt),
		})
	}
	return out, nil
}

func (b *credentialBroker) Request(ctx context.Context, body agentcreds.RequestBody) (agentcreds.RequestStatus, error) {
	uses := make([]credentialUseDoc, 0, len(body.Uses))
	for _, use := range body.Uses {
		uses = append(uses, credentialUseDoc{Description: use.Description})
	}
	doc, err := b.controlPlan.createRequest(ctx, createCredentialRequestDoc{
		SandboxID:     b.sandboxID,
		Name:          body.Name,
		EnvVar:        body.EnvVar,
		Host:          body.Host,
		Justification: body.Justification,
		Uses:          uses,
	})
	if err != nil {
		return agentcreds.RequestStatus{}, err
	}
	return agentcreds.RequestStatus{RequestID: doc.RequestID, Status: doc.Status, Uses: protocolUses(doc.Uses, nil)}, nil
}

func (b *credentialBroker) RequestStatus(ctx context.Context, requestID string) (agentcreds.RequestStatus, error) {
	doc, err := b.controlPlan.requestStatus(ctx, b.sandboxID, requestID)
	if err != nil {
		return agentcreds.RequestStatus{}, err
	}
	return agentcreds.RequestStatus{RequestID: doc.RequestID, Status: doc.Status, Uses: protocolUses(doc.Uses, nil)}, nil
}

// Get mints one ephemeral sentinel for one approved use.
//
// It re-reads the credential from the control plane rather than trusting a
// cache: the answer to "may this sandbox still use this?" is the control
// plane's, and a revoked grant must stop producing activations immediately
// rather than at the end of some local TTL.
func (b *credentialBroker) Get(ctx context.Context, body agentcreds.UseBody) (agentcreds.UseResponse, error) {
	useID := strings.TrimSpace(body.UseID)
	if useID == "" {
		return agentcreds.UseResponse{}, fmt.Errorf("%w: useId is required", agentcreds.ErrInvalid)
	}
	docs, err := b.controlPlan.list(ctx, b.sandboxID)
	if err != nil {
		return agentcreds.UseResponse{}, err
	}
	for _, doc := range docs {
		for _, use := range doc.Uses {
			if use.UseID != useID {
				continue
			}
			record, err := b.activations.mint(b.sandboxID, doc.Sentinel, useID, doc.Host, doc.Format, body.Command)
			if err != nil {
				return agentcreds.UseResponse{}, err
			}
			expiresAt := record.ExpiresAt
			// The grant is the consent clock and the activation is the use
			// clock; the value dies at whichever comes first.
			if doc.ExpiresAt != nil && doc.ExpiresAt.Before(expiresAt) {
				expiresAt = *doc.ExpiresAt
			}
			return agentcreds.UseResponse{EnvVar: doc.EnvVar, Value: record.Sentinel, ExpiresAt: &expiresAt}, nil
		}
	}
	// Unknown, revoked, or expired all look the same from here, and saying which
	// would tell an untrusted caller more than it needs.
	return agentcreds.UseResponse{}, fmt.Errorf("%w: no live approved use %s", agentcreds.ErrDenied, useID)
}

func protocolUses(docs []credentialUseDoc, expiresAt *time.Time) []agentcreds.Use {
	if len(docs) == 0 {
		return nil
	}
	out := make([]agentcreds.Use, 0, len(docs))
	for _, doc := range docs {
		out = append(out, agentcreds.Use{UseID: doc.UseID, Description: doc.Description, ExpiresAt: expiresAt})
	}
	return out
}
