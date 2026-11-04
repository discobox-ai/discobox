package proxyagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/proxy"
)

// The pool half of the agent credentials protocol (ADR 0031): it relays a
// sandbox's list and requests to the control plane, and answers its host
// trusts and egress audit from this process (ADR 0149, ADR 26-10-02-678). It
// hands out no values: a credential granted to the sandbox is delivered as an
// injected sentinel like any other (ADR 26-10-27-880).
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

// credentialDoc is one credential the control plane says a sandbox holds.
// Purpose is read when the control plane says it: a delegate grant, which is
// nothing the sandbox is delivered.
type credentialDoc struct {
	Name     string   `json:"name"`
	EnvVar   string   `json:"envVar"`
	Hosts    []string `json:"hosts,omitempty"`
	SecretID string   `json:"secretId"`
	GrantID  string   `json:"grantId"`
	Purpose  string   `json:"purpose,omitempty"`
	// Raw is how the sandbox holds the value itself, when it does rather
	// than a sentinel (ADR 26-10-27-840).
	Raw       string     `json:"raw,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// Grants are, for a use, each grant its sentinel is held to (ADR
	// 26-11-03-329 §3); a control plane from before grants sends none.
	Grants []credentialGrantDoc `json:"grants,omitempty"`
}

// credentialGrantDoc is one grant of a credential as the control plane lists
// it.
type credentialGrantDoc struct {
	Hosts     []string   `json:"hosts"`
	Adds      bool       `json:"adds,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

type listCredentialsDoc struct {
	Credentials []credentialDoc `json:"credentials"`
}

// createCredentialRequestDoc is an ask as the control plane takes it: a use
// part (EnvVar with Hosts, Routes or Policies), a delegate part, or both (ADR
// 26-11-02-022 §1). Policies and the delegate part are the protocol's own
// types, whose JSON is the control plane's too, so each policy document is
// carried as the bytes the sandbox sent and never re-encoded here.
type createCredentialRequestDoc struct {
	SandboxID       string                   `json:"sandboxId"`
	Name            string                   `json:"name"`
	EnvVar          string                   `json:"envVar,omitempty"`
	Add             bool                     `json:"add,omitempty"`
	Hosts           []string                 `json:"hosts,omitempty"`
	Routes          []string                 `json:"routes,omitempty"`
	Policies        []agentcreds.Policy      `json:"policies,omitempty"`
	Delegate        *agentcreds.DelegatePart `json:"delegate,omitempty"`
	Justification   string                   `json:"justification,omitempty"`
	GrantTTLSeconds int64                    `json:"grantTTLSeconds,omitempty"`
}

// credentialRequestStatusDoc is a request's status as the control plane
// answers it: pending until answered, then granted when any part was, and
// each part the request has.
type credentialRequestStatusDoc struct {
	RequestID string                          `json:"requestId"`
	Status    string                          `json:"status"`
	Use       *credentialRequestPartStatusDoc `json:"use,omitempty"`
	Delegate  *credentialRequestPartStatusDoc `json:"delegate,omitempty"`
}

// credentialRequestPartStatusDoc is one part of a request. Name, EnvVar and
// Hosts are what was granted, read when the control plane says them.
type credentialRequestPartStatusDoc struct {
	Status    string     `json:"status"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	Name      string     `json:"name,omitempty"`
	EnvVar    string     `json:"envVar,omitempty"`
	Hosts     []string   `json:"hosts,omitempty"`
	// Raw is, once the use part is granted, the delivery of the value itself
	// the approver chose (ADR 26-10-27-840 §2), so the agent is told it holds
	// the value.
	Raw string `json:"raw,omitempty"`
	// GrantedAs is, once the use part is granted, whether as an addition or
	// a replacement (ADR 26-11-03-329 §4).
	GrantedAs string `json:"grantedAs,omitempty"`
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
	// secretsPath is SecretsFile, which holds each sandbox's credential
	// environment as the pool agent last delivered it.
	secretsPath string
	trusts      *hostTrusts
	// proxy is the server this process runs, whose audit trail the broker
	// reads this sandbox's rows from.
	proxy *proxy.Server
}

var _ agentcreds.Service = (*credentialBroker)(nil)

func (b *credentialBroker) List(ctx context.Context) ([]agentcreds.Credential, error) {
	docs, err := b.controlPlan.list(ctx, b.sandboxID)
	if err != nil {
		return nil, err
	}
	out := make([]agentcreds.Credential, 0, len(docs))
	for _, doc := range docs {
		credential := agentcreds.Credential{
			Name:      doc.Name,
			EnvVar:    doc.EnvVar,
			Hosts:     doc.Hosts,
			Purpose:   doc.Purpose,
			Raw:       doc.Raw,
			ExpiresAt: doc.ExpiresAt,
		}
		for _, grant := range doc.Grants {
			credential.Grants = append(credential.Grants, agentcreds.CredentialGrant{Hosts: grant.Hosts, Adds: grant.Adds, ExpiresAt: grant.ExpiresAt})
		}
		out = append(out, credential)
	}
	return out, nil
}

// Environment answers with every variable a credential is delivered in for
// this sandbox, and the sentinel delivered there, as the pool agent last
// delivered them (secretsDoc.Environment). A sentinel is a placeholder only
// this sandbox's egress exchanges, never the credential; the sandbox already
// holds each one in its secret environment, which this re-reads for a process
// started before it changed.
//
// A raw secret delivered in a file (ADR 26-10-27-840) is its variable and the
// file's path, which is all its variable ever holds; one delivered as its
// value is left out, since the value is the one thing this never hands a
// process that asks.
func (b *credentialBroker) Environment(context.Context) (map[string]string, error) {
	doc, err := readSecretsDoc(b.secretsPath)
	if err != nil {
		return nil, err
	}
	env := maps.Clone(doc.Environment[b.sandboxID])
	for _, raw := range doc.Raw[b.sandboxID] {
		if raw.Delivery != sandboxconfig.RawDeliveryFile {
			continue
		}
		if env == nil {
			env = map[string]string{}
		}
		env[raw.EnvName] = sandboxconfig.RawSecretPath(raw.EnvName)
	}
	return env, nil
}

func (b *credentialBroker) Request(ctx context.Context, body agentcreds.RequestBody) (agentcreds.RequestStatus, error) {
	doc, err := b.controlPlan.createRequest(ctx, createCredentialRequestDoc{
		SandboxID:       b.sandboxID,
		Name:            body.Name,
		EnvVar:          body.EnvVar,
		Add:             body.Add,
		Hosts:           body.Hosts,
		Routes:          body.Routes,
		Policies:        body.Policies,
		Delegate:        body.Delegate,
		Justification:   body.Justification,
		GrantTTLSeconds: body.GrantTTLSeconds,
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

// protocol is the status as the sandbox is answered with it: each part the
// request has, and what the sandbox now holds under one once granted.
func (d credentialRequestStatusDoc) protocol() agentcreds.RequestStatus {
	return agentcreds.RequestStatus{
		RequestID: d.RequestID,
		Status:    d.Status,
		Use:       d.Use.protocol(agentcreds.PurposeUse),
		Delegate:  d.Delegate.protocol(agentcreds.PurposeDelegate),
	}
}

func (d *credentialRequestPartStatusDoc) protocol(purpose string) *agentcreds.PartStatus {
	if d == nil {
		return nil
	}
	part := &agentcreds.PartStatus{Status: d.Status}
	if d.Status == agentcreds.StatusGranted {
		part.GrantedAs = d.GrantedAs
		part.Credential = &agentcreds.Credential{
			Name:      d.Name,
			EnvVar:    d.EnvVar,
			Hosts:     d.Hosts,
			Purpose:   purpose,
			Raw:       d.Raw,
			ExpiresAt: d.ExpiresAt,
		}
	}
	return part
}
