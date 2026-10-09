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
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/wellknown"
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

// credentialUseTimeout bounds a use, which waits on the project's judge before
// anything is minted (ADR 26-09-22-838 §3). It allows the judge's whole
// deadline and the calls around it, and stays inside what the sandbox waits
// (agentcreds.UseTimeout), so a judge that takes its time is answered with its
// sentence rather than the sandbox giving up first.
const credentialUseTimeout = judgeHTTPTimeout + credentialBrokerTimeout

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
	Hosts     []string           `json:"hosts,omitempty"`
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

// commandAskDoc is a command a sandbox is about to run, as the control plane
// is asked about it (ADR 26-09-22-838 §3). Everything in it but the sandbox
// is the sandbox's word.
type commandAskDoc struct {
	SandboxID string          `json:"sandboxId"`
	UseID     string          `json:"useId"`
	Command   []string        `json:"command"`
	Stdin     *judge.Input    `json:"stdin,omitempty"`
	Reported  *judge.Reported `json:"reported,omitempty"`
}

type createCredentialRequestDoc struct {
	SandboxID       string             `json:"sandboxId"`
	ID              string             `json:"id,omitempty"`
	Name            string             `json:"name"`
	EnvVar          string             `json:"envVar"`
	Hosts           []string           `json:"hosts,omitempty"`
	Justification   string             `json:"justification,omitempty"`
	Uses            []credentialUseDoc `json:"uses"`
	GrantTTLSeconds int64              `json:"grantTTLSeconds,omitempty"`
	Purpose         string             `json:"purpose,omitempty"`
}

type credentialRequestStatusDoc struct {
	RequestID string             `json:"requestId"`
	Status    string             `json:"status"`
	Purpose   string             `json:"purpose,omitempty"`
	Uses      []credentialUseDoc `json:"uses,omitempty"`
	ExpiresAt *time.Time         `json:"expiresAt,omitempty"`
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
	// judge reaches the same control plane with a client that waits as long
	// as a verdict takes.
	judge       *controlPlaneCredentials
	activations *activations
	trusts      *hostTrusts
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
			Hosts:  doc.Hosts,
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
	// A well-known credential says its own name, variable, and hosts. What
	// the agent spelled out is passed on as it was sent, not replaced: the
	// control plane fills what was left out and refuses what contradicts the
	// ID, and it can only refuse what it is shown.
	if _, ok := wellknown.Lookup(body.ID); body.ID != "" && !ok {
		return agentcreds.RequestStatus{}, fmt.Errorf("%w: %q is not a well-known credential", agentcreds.ErrInvalid, body.ID)
	}
	doc, err := b.controlPlan.createRequest(ctx, createCredentialRequestDoc{
		SandboxID:       b.sandboxID,
		ID:              body.ID,
		Name:            body.Name,
		EnvVar:          body.EnvVar,
		Hosts:           body.Hosts,
		Justification:   body.Justification,
		Uses:            uses,
		GrantTTLSeconds: body.GrantTTLSeconds,
		Purpose:         body.Purpose,
	})
	if err != nil {
		return agentcreds.RequestStatus{}, err
	}
	return doc.protocol(), nil
}

func (b *credentialBroker) RequestStatus(ctx context.Context, requestID string) (agentcreds.RequestStatus, error) {
	doc, err := b.controlPlan.requestStatus(ctx, b.sandboxID, requestID)
	if err != nil {
		return agentcreds.RequestStatus{}, err
	}
	return doc.protocol(), nil
}

// protocol is the status as the sandbox is answered with it.
func (d credentialRequestStatusDoc) protocol() agentcreds.RequestStatus {
	return agentcreds.RequestStatus{RequestID: d.RequestID, Status: d.Status, Purpose: d.Purpose, Uses: protocolUses(d.Uses, d.ExpiresAt)}
}

// Get mints one ephemeral sentinel for one approved use, once the project's
// judge has allowed the command it is for (ADR 26-09-22-838 §3).
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
	if len(body.Command) == 0 || strings.TrimSpace(body.Command[0]) == "" {
		return agentcreds.UseResponse{}, fmt.Errorf("%w: command is required: a value is only handed out for a command to judge", agentcreds.ErrInvalid)
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
			// Judged before the mint, and gating it. The control plane records
			// the verdict before it answers, so nothing is minted for a verdict
			// that is not on record (ADR 0091).
			if err := b.judgeCommand(ctx, useID, body); err != nil {
				return agentcreds.UseResponse{}, err
			}
			record, err := b.activations.mint(b.sandboxID, doc.Sentinel, useID, doc.Hosts, doc.Format, body.Command)
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

// judgeCommand asks the project's judge whether the command carries out the
// use, and returns nil only for an explicit allow, or for a server that says
// it does not judge commands (ADR 26-10-02-054 §3). Everything else refuses:
// a refusal, a judge that could not be reached, an answer that could not be
// read. Unlike a request, no answer from a server that judges never lets a
// command through, because a command is asked about before anything exists
// to lose.
func (b *credentialBroker) judgeCommand(ctx context.Context, useID string, body agentcreds.UseBody) error {
	ask := commandAskDoc{SandboxID: b.sandboxID, UseID: useID, Command: body.Command}
	if in := body.Stdin; in != nil {
		ask.Stdin = &judge.Input{Content: in.Content, Missing: in.Missing}
	}
	if r := body.Reported; r != nil {
		ask.Reported = &judge.Reported{
			WorkingDirectory: r.WorkingDirectory,
			RepositoryRoot:   r.RepositoryRoot,
			RefCommit:        r.RefCommit,
			RefSubject:       r.RefSubject,
		}
	}
	answer, err := b.judge.askJudge(ctx, "judge-commands", ask)
	switch {
	case err == nil && answer.Allow != nil && *answer.Allow:
		return nil
	case err == nil:
		reason := strings.TrimSpace(answer.Reason)
		if reason == "" {
			reason = "the judge did not allow the command"
		}
		return fmt.Errorf("%w: %s", agentcreds.ErrDenied, reason)
	case outcomeOf(err) == outcomeNobodyJudges:
		return nil
	default:
		return fmt.Errorf("%w: the command could not be judged, so nothing was issued for it: %w", agentcreds.ErrDenied, err)
	}
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
