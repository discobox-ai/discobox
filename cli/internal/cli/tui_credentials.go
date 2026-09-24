package cli

import (
	"context"
	"sort"
	"strings"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// The window's half of the credential inbox: the requests waiting on a person,
// the secrets one of them can be answered with, and the two answers.
//
// Everything here is the same API the `discobox secret` commands use. The
// window adds no shortcut of its own — an approval is the same act however it
// is made, and a second path to it that skipped a check would be the one worth
// exploiting.

// CredentialRequests returns the pending requests on every server the window
// lists, newest first, each naming its server (ADR 0131 §1).
//
// It is polled on the listing's beat, and on the listing's terms: a server that
// is slow is left in flight rather than waited on, and one that fails has
// nothing waiting on it as far as the window can tell. That is said where its
// discoboxes are when the listing failed too. A server that lists and refuses
// this call alone is not called out: its requests are missing until it answers
// again, which is the cost of not reporting a polled failure on every beat. Each request is on the same leash the listing puts
// its requests on (pollTimeout): long enough that a slow server's answer still
// arrives, short enough that a request which will never come back is not left
// outstanding for the life of the window.
func (d *apiDataSource) CredentialRequests(ctx context.Context) ([]tui.CredentialRequest, error) {
	if d.servers == nil {
		ctx, cancel := context.WithTimeout(ctx, pollTimeout)
		defer cancel()
		return d.requestsHere(ctx)
	}
	pollEveryServer(ctx, d, func(s *tuiServer) *serverPoll[tui.CredentialRequest] { return &s.requests },
		func(ctx context.Context, source *apiDataSource) ([]tui.CredentialRequest, error) {
			return source.requestsHere(ctx)
		})

	d.mu.Lock()
	defer d.mu.Unlock()
	var out []tui.CredentialRequest
	// A request two servers both list is one server registered under two
	// addresses, and is listed once, under the first — as its discobox is.
	seen := map[string]bool{}
	for _, s := range d.servers {
		for _, req := range s.requests.last {
			if seen[req.ID] {
				continue
			}
			seen[req.ID] = true
			req.Server = s.name
			out = append(out, req)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// answered tells the inbox poll of the server named that one of its requests
// has just been answered, which is what the window re-reads the inbox for. The
// request goes from what the poll holds, and anything it had in flight — asked
// before the answer — is discarded when it lands, so the mark and the banner
// go with the request however slow the server is to list again.
func (d *apiDataSource) answered(server, requestID string) {
	if d.servers == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.servers {
		if s.name == server {
			stale(&s.requests, func(r tui.CredentialRequest) bool { return r.ID == requestID })
			return
		}
	}
}

// requestsHere is this data source's own server's pending requests, credential
// and trust alike: the server's inbox read (ADR 0149 §7), so each beat still
// makes one call to it.
func (d *apiDataSource) requestsHere(ctx context.Context) ([]tui.CredentialRequest, error) {
	res, err := d.client.ListApprovalRequests(ctx, apiclientgen.ListApprovalRequestsParams{
		ProjectId: d.projectID,
		Status:    apiclientgen.NewOptListApprovalRequestsStatus(apiclientgen.ListApprovalRequestsStatusPending),
	})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListApprovalRequestsBody](res)
	if err != nil {
		return nil, err
	}
	out := make([]tui.CredentialRequest, 0, len(body.GetApprovalRequests()))
	for _, item := range body.GetApprovalRequests() {
		if credential, ok := item.Credential.Get(); ok && item.Kind == apiclientgen.ApprovalRequestKindCredential {
			out = append(out, toTUICredentialRequest(credential))
		}
		if trust, ok := item.Trust.Get(); ok && item.Kind == apiclientgen.ApprovalRequestKindTrust {
			out = append(out, toTUITrustRequest(trust))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// SecretRejections is what the window knows does not work: credentials an
// upstream refused that their server could not renew (ADR 0132), on every
// server the window lists, the way the credential inbox is (ADR 0131 §1).
func (d *apiDataSource) SecretRejections(ctx context.Context) ([]tui.SecretRejection, error) {
	if d.servers == nil {
		ctx, cancel := context.WithTimeout(ctx, pollTimeout)
		defer cancel()
		return d.rejectionsHere(ctx)
	}
	pollEveryServer(ctx, d, func(s *tuiServer) *serverPoll[tui.SecretRejection] { return &s.rejections },
		func(ctx context.Context, source *apiDataSource) ([]tui.SecretRejection, error) {
			return source.rejectionsHere(ctx)
		})

	d.mu.Lock()
	defer d.mu.Unlock()
	var out []tui.SecretRejection
	// A server registered under two addresses lists the same rows twice; they
	// are kept once, under the first, as its discoboxes and requests are.
	seen := map[string]bool{}
	for _, s := range d.servers {
		for _, rejection := range s.rejections.last {
			key := rejection.SecretID + "\x00" + rejection.Host
			if seen[key] {
				continue
			}
			seen[key] = true
			rejection.Server = s.name
			out = append(out, rejection)
		}
	}
	return out, nil
}

// rejectionsHere is one server's refused credentials.
func (d *apiDataSource) rejectionsHere(ctx context.Context) ([]tui.SecretRejection, error) {
	res, err := d.client.ListSecretRejections(ctx, apiclientgen.ListSecretRejectionsParams{ProjectId: d.projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListSecretRejectionsBody](res)
	if err != nil {
		return nil, err
	}
	rejections := body.GetSecretRejections()
	out := make([]tui.SecretRejection, 0, len(rejections))
	for _, r := range rejections {
		out = append(out, tui.SecretRejection{
			SecretID:          r.SecretId,
			SecretName:        strings.TrimSpace(r.SecretName.Or("")),
			SecretType:        string(r.SecretType.Or("")),
			Host:              strings.TrimSpace(r.Host),
			Reason:            string(r.Reason),
			EnvName:           strings.TrimSpace(r.EnvName.Or("")),
			SandboxID:         strings.TrimSpace(r.SandboxId.Or("")),
			HarnessConfigID:   strings.TrimSpace(r.HarnessConfigId.Or("")),
			HarnessConfigName: strings.TrimSpace(r.HarnessConfigName.Or("")),
			FirstSeen:         r.FirstSeenAt,
			LastSeen:          r.LastSeenAt,
		})
	}
	return out, nil
}

func toTUICredentialRequest(r apimodel.SecretRequest) tui.CredentialRequest {
	req := tui.CredentialRequest{
		ID:            r.ID,
		SandboxID:     strings.TrimSpace(r.SandboxId.Or("")),
		Name:          strings.TrimSpace(r.Name.Or("")),
		EnvVar:        strings.TrimSpace(r.EnvName.Or("")),
		Host:          strings.TrimSpace(r.Host.Or("")),
		Type:          string(r.Type),
		Justification: strings.TrimSpace(r.Justification.Or("")),
		GrantTTL:      lifetime.FromRequest(r.GrantTTLSeconds.Or(0)),
		Delegate:      r.Purpose.Or("") == apiclientgen.SecretRequestPurposeDelegate,
		WellKnownID:   strings.TrimSpace(r.WellKnownId.Or("")),
		Created:       r.CreatedAt,
	}
	if uses, ok := r.Uses.Get(); ok {
		for _, use := range uses {
			if description := strings.TrimSpace(use.Description); description != "" {
				req.Uses = append(req.Uses, description)
			}
		}
	}
	return req
}

// toTUITrustRequest is a trust request as the inbox holds it: an item whose
// answer is a pin rather than a secret.
func toTUITrustRequest(r apimodel.HostTrustRequest) tui.CredentialRequest {
	ask := &tui.TrustAsk{}
	for _, cert := range r.ObservedChain.Or(nil) {
		ask.Chain = append(ask.Chain, toTUITrustCertificate(cert))
	}
	if supplied, ok := r.SuppliedCA.Get(); ok {
		ca := toTUITrustCertificate(supplied)
		ask.SuppliedCA = &ca
	}
	ask.DefaultPin = defaultTrustPin(*ask)
	req := tui.CredentialRequest{
		ID:            r.ID,
		SandboxID:     r.SandboxId,
		Host:          r.Host,
		Justification: strings.TrimSpace(r.Justification.Or("")),
		GrantTTL:      lifetime.FromRequest(r.GrantTTLSeconds.Or(0)),
		Created:       r.CreatedAt,
		Trust:         ask,
	}
	for _, use := range r.Uses.Or(nil) {
		if description := strings.TrimSpace(use.Description); description != "" {
			req.Uses = append(req.Uses, description)
		}
	}
	return req
}

func toTUITrustCertificate(cert apimodel.ObservedCertificate) tui.TrustCertificate {
	return tui.TrustCertificate{
		Subject:    cert.Subject,
		Issuer:     cert.Issuer,
		Names:      append(cert.DnsNames.Or(nil), cert.Ips.Or(nil)...),
		NotBefore:  cert.NotBefore,
		NotAfter:   cert.NotAfter,
		SHA256:     cert.SHA256,
		SPKISHA256: cert.SpkiSha256,
		IsCA:       cert.IsCA.Or(false),
		SelfSigned: cert.SelfSigned.Or(false),
	}
}

// defaultTrustPin is the pin the server takes when an approval names none —
// the supplied CA, then a self-signed CA in the chain, then the leaf's key —
// so the window offers first what `discobox trust request approve` would
// pin without --pin.
func defaultTrustPin(ask tui.TrustAsk) tui.TrustPin {
	if ask.SuppliedCA != nil {
		return tui.TrustPin{Kind: tui.TrustPinCA, SHA256: ask.SuppliedCA.SHA256}
	}
	for i := len(ask.Chain) - 1; i >= 0; i-- {
		if cert := ask.Chain[i]; cert.IsCA && cert.SelfSigned {
			return tui.TrustPin{Kind: tui.TrustPinCA, SHA256: cert.SHA256}
		}
	}
	if len(ask.Chain) == 0 {
		return tui.TrustPin{}
	}
	return tui.TrustPin{Kind: tui.TrustPinLeafSPKI, SHA256: ask.Chain[0].SPKISHA256}
}

// ApproveTrustRequest pins the chosen certificate for the asking discobox.
// The lifetime is always sent: a trust always lapses.
func (d *apiDataSource) ApproveTrustRequest(ctx context.Context, server string, approval tui.TrustApproval) error {
	window := d
	d, err := d.on(ctx, server)
	if err != nil {
		return err
	}
	body := &apimodel.ApproveTrustRequestBody{}
	body.SetGrantTTLSeconds(apiclientgen.NewOptInt64(approval.TTLSeconds))
	body.SetPin(apiclientgen.NewOptTrustPin(apimodel.TrustPin{Kind: apiclientgen.TrustPinKind(approval.Pin.Kind), SHA256: approval.Pin.SHA256}))
	res, err := d.client.ApproveTrustRequest(ctx, body, apiclientgen.ApproveTrustRequestParams{
		ProjectId: d.projectID,
		RequestId: approval.RequestID,
	})
	if err != nil {
		return err
	}
	if _, err := expectResponse[apimodel.HostTrustRequest](res); err != nil {
		return err
	}
	window.answered(server, approval.RequestID)
	return nil
}

// DenyTrustRequest answers a trust request no.
func (d *apiDataSource) DenyTrustRequest(ctx context.Context, server, requestID string) error {
	window := d
	d, err := d.on(ctx, server)
	if err != nil {
		return err
	}
	res, err := d.client.DenyTrustRequest(ctx, apiclientgen.DenyTrustRequestParams{
		ProjectId: d.projectID,
		RequestId: requestID,
	})
	if err != nil {
		return err
	}
	if err := expectNoContent[apiclientgen.DenyTrustRequestNoContent](res); err != nil {
		return err
	}
	window.answered(server, requestID)
	return nil
}

// Secrets returns one server's secrets, without values.
func (d *apiDataSource) Secrets(ctx context.Context, server string) ([]tui.Secret, error) {
	d, err := d.on(ctx, server)
	if err != nil {
		return nil, err
	}
	res, err := d.client.ListSecrets(ctx, apiclientgen.ListSecretsParams{ProjectId: d.projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListSecretsBody](res)
	if err != nil {
		return nil, err
	}
	secrets := sortedByRecency(body.GetSecrets(), func(s apimodel.Secret) time.Time { return s.UpdatedAt })
	out := make([]tui.Secret, 0, len(secrets))
	for _, s := range secrets {
		row := tui.Secret{
			ID:          s.ID,
			Name:        s.Name,
			Type:        string(s.Type),
			Host:        strings.TrimSpace(s.Host.Or("")),
			MaxTTL:      time.Duration(s.MaxGrantTTLSeconds) * time.Second,
			WellKnownID: strings.TrimSpace(s.WellKnownId.Or("")),
			Created:     s.CreatedAt,
			Updated:     s.UpdatedAt,
		}
		if oauth, ok := s.OAuth.Get(); ok {
			row.OAuth = &tui.SecretOAuth{
				TokenURL:         strings.TrimSpace(oauth.TokenUrl.Or("")),
				ClientID:         strings.TrimSpace(oauth.ClientId.Or("")),
				SubscriptionType: strings.TrimSpace(oauth.SubscriptionType.Or("")),
				Refreshable:      oauth.Refreshable.Or(false),
			}
			if scopes, ok := oauth.Scopes.Get(); ok {
				row.OAuth.Scopes = scopes
			}
			if expires := oauth.AccessTokenExpiresAt.Or(0); expires > 0 {
				row.OAuth.AccessTokenExpiresAt = time.UnixMilli(expires).UTC()
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// CreateSecret stores a credential typed into the approval dialog.
func (d *apiDataSource) CreateSecret(ctx context.Context, server string, secret tui.NewSecret) (tui.Secret, error) {
	d, err := d.on(ctx, server)
	if err != nil {
		return tui.Secret{}, err
	}
	kind := strings.TrimSpace(secret.Type)
	if kind == "" {
		kind = "token"
	}
	secretType, err := createSecretBodyType(kind)
	if err != nil {
		return tui.Secret{}, err
	}
	body := &apimodel.CreateSecretBody{Name: strings.TrimSpace(secret.Name), Type: secretType}
	if host := strings.TrimSpace(secret.Host); host != "" {
		body.SetHost(apiclientgen.NewOptString(host))
	}
	// Always sent, zero included: zero is the answer "grants on this may live
	// forever", and leaving it out would take the server's default instead.
	body.SetMaxGrantTTLSeconds(apiclientgen.NewOptInt64(secret.MaxTTLSeconds))
	body.Value = secretValueBody(secret.Value)

	res, err := d.client.CreateSecret(ctx, body, apiclientgen.CreateSecretParams{ProjectId: d.projectID})
	if err != nil {
		return tui.Secret{}, err
	}
	created, err := expectResponse[apimodel.Secret](res)
	if err != nil {
		return tui.Secret{}, err
	}
	return tui.Secret{
		ID:     created.ID,
		Name:   created.Name,
		Type:   string(created.Type),
		Host:   strings.TrimSpace(created.Host.Or("")),
		MaxTTL: time.Duration(created.MaxGrantTTLSeconds) * time.Second,
	}, nil
}

// secretValueBody is a credential's material on the wire. The optional halves
// are sent only when they are there: an empty refresh token on a token secret
// is not a field the server has any use for.
func secretValueBody(value tui.SecretValue) apimodel.SecretValue {
	body := apimodel.SecretValue{Token: apiclientgen.NewOptString(value.Token)}
	if value.RefreshToken != "" {
		body.SetRefreshToken(apiclientgen.NewOptString(value.RefreshToken))
	}
	if value.TokenURL != "" {
		body.SetTokenUrl(apiclientgen.NewOptString(value.TokenURL))
	}
	if value.ClientID != "" {
		body.SetClientId(apiclientgen.NewOptString(value.ClientID))
	}
	if len(value.Scopes) > 0 {
		body.SetScopes(apiclientgen.NewOptNilStringArray(value.Scopes))
	}
	return body
}

// UpdateSecret changes what a secret says about itself, in one call. Every
// field the update names is set even when it is empty or zero — an unset field
// changes nothing, so releasing a binding and lifting a limit both have to be
// said rather than left out — and every field it does not name is left out.
func (d *apiDataSource) UpdateSecret(ctx context.Context, server, secretID string, update tui.SecretUpdate) error {
	d, err := d.on(ctx, server)
	if err != nil {
		return err
	}
	body := &apimodel.UpdateSecretBody{}
	if update.Name != nil {
		body.SetName(apiclientgen.NewOptString(strings.TrimSpace(*update.Name)))
	}
	if update.Host != nil {
		body.SetHost(apiclientgen.NewOptString(strings.TrimSpace(*update.Host)))
	}
	if update.MaxTTLSeconds != nil {
		body.SetMaxGrantTTLSeconds(apiclientgen.NewOptInt64(*update.MaxTTLSeconds))
	}
	if update.Value != nil {
		body.SetValue(apiclientgen.NewOptSecretValue(secretValueBody(*update.Value)))
	}
	res, err := d.client.UpdateSecret(ctx, body, apiclientgen.UpdateSecretParams{
		ProjectId: d.projectID,
		SecretId:  secretID,
	})
	if err != nil {
		return err
	}
	_, err = expectResponse[apimodel.Secret](res)
	return err
}

// ApproveCredentialRequest mints the grant that answers a request.
func (d *apiDataSource) ApproveCredentialRequest(ctx context.Context, server string, approval tui.Approval) error {
	window := d
	d, err := d.on(ctx, server)
	if err != nil {
		return err
	}
	// The lifetime is always sent, zero included: zero is a grant that never
	// expires, and leaving it out would ask the server for the secret's own
	// limit instead — a different grant from the one the window said it was
	// minting.
	body := &apimodel.ApproveSecretRequestBody{SecretId: apiclientgen.NewOptString(approval.SecretID)}
	body.SetGrantTTLSeconds(apiclientgen.NewOptInt64(approval.TTLSeconds))
	if approval.SecretHost != nil {
		body.SetSecretHost(apiclientgen.NewOptString(strings.TrimSpace(*approval.SecretHost)))
	}
	if approval.SecretMaxTTLSeconds != nil {
		body.SetSecretMaxGrantTTLSeconds(apiclientgen.NewOptInt64(*approval.SecretMaxTTLSeconds))
	}
	res, err := d.client.ApproveSecretRequest(ctx, body, apiclientgen.ApproveSecretRequestParams{
		ProjectId: d.projectID,
		RequestId: approval.RequestID,
	})
	if err != nil {
		return err
	}
	if _, err := expectResponse[apimodel.SecretRequest](res); err != nil {
		return err
	}
	window.answered(server, approval.RequestID)
	return nil
}

// DenyCredentialRequest answers a request no.
func (d *apiDataSource) DenyCredentialRequest(ctx context.Context, server, requestID string) error {
	window := d
	d, err := d.on(ctx, server)
	if err != nil {
		return err
	}
	res, err := d.client.DenySecretRequest(ctx, apiclientgen.DenySecretRequestParams{
		ProjectId: d.projectID,
		RequestId: requestID,
	})
	if err != nil {
		return err
	}
	if err := expectNoContent[apiclientgen.DenySecretRequestNoContent](res); err != nil {
		return err
	}
	window.answered(server, requestID)
	return nil
}

// Grants lists the standing grants on a secret, or on one server's whole
// project.
func (d *apiDataSource) Grants(ctx context.Context, server, secretID string) ([]tui.Grant, error) {
	d, err := d.on(ctx, server)
	if err != nil {
		return nil, err
	}
	params := apiclientgen.ListSecretGrantsParams{ProjectId: d.projectID}
	if secretID = strings.TrimSpace(secretID); secretID != "" {
		params.SecretId = apiclientgen.NewOptString(secretID)
	}
	res, err := d.client.ListSecretGrants(ctx, params)
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListSecretGrantsBody](res)
	if err != nil {
		return nil, err
	}
	grants := sortedByRecency(body.GetSecretGrants(), func(g apimodel.SecretGrant) time.Time { return g.GrantedAt })
	out := make([]tui.Grant, 0, len(grants))
	for _, g := range grants {
		grant := tui.Grant{
			ID:        g.ID,
			SecretID:  g.SecretId,
			Scope:     string(g.Scope),
			ScopeKey:  strings.TrimSpace(g.ScopeKey),
			Host:      strings.TrimSpace(g.Host.Or("")),
			Delegate:  g.Purpose == apiclientgen.SecretGrantPurposeDelegate,
			GrantedBy: strings.TrimSpace(g.GrantedBy.Or("")),
			Granted:   g.GrantedAt,
		}
		if expires, ok := g.ExpiresAt.Get(); ok {
			grant.Expires = expires
		}
		if uses, ok := g.Uses.Get(); ok {
			for _, use := range uses {
				if description := strings.TrimSpace(use.Description); description != "" {
					grant.Uses = append(grant.Uses, tui.GrantUse{
						ID:          strings.TrimSpace(use.UseId.Or("")),
						Description: description,
					})
				}
			}
		}
		out = append(out, grant)
	}
	return out, nil
}

// CreateGrant mints a standing grant.
func (d *apiDataSource) CreateGrant(ctx context.Context, server string, grant tui.NewGrant) (tui.Grant, error) {
	d, err := d.on(ctx, server)
	if err != nil {
		return tui.Grant{}, err
	}
	scope, err := createSecretGrantBodyScope(grant.Scope)
	if err != nil {
		return tui.Grant{}, err
	}
	body := &apimodel.CreateSecretGrantBody{SecretId: grant.SecretID, Scope: scope}
	if grant.ScopeKey != "" {
		body.SetScopeKey(apiclientgen.NewOptString(grant.ScopeKey))
	}
	// Set even when empty: an unset host takes the secret's own binding, and
	// "anywhere the secret allows" has to be sayable.
	body.SetHost(apiclientgen.NewOptString(grant.Host))
	// Said even when zero: the window asked how long it lives and was answered,
	// and zero is the answer "never expires" — dropping it would quietly
	// substitute the secret's limit for what the person typed.
	body.SetGrantTTLSeconds(apiclientgen.NewOptInt64(grant.TTLSeconds))
	if len(grant.Uses) > 0 {
		declared := make([]apimodel.SecretUse, 0, len(grant.Uses))
		for _, use := range grant.Uses {
			declared = append(declared, apimodel.SecretUse{Description: use})
		}
		body.SetUses(apiclientgen.NewOptNilSecretUseArray(declared))
		body.SetEnvVar(apiclientgen.NewOptString(grant.EnvVar))
	}
	res, err := d.client.CreateSecretGrant(ctx, body, apiclientgen.CreateSecretGrantParams{ProjectId: d.projectID})
	if err != nil {
		return tui.Grant{}, err
	}
	created, err := expectResponse[apimodel.SecretGrant](res)
	if err != nil {
		return tui.Grant{}, err
	}
	return tui.Grant{
		ID:       created.ID,
		SecretID: created.SecretId,
		Scope:    string(created.Scope),
		ScopeKey: strings.TrimSpace(created.ScopeKey),
		Host:     strings.TrimSpace(created.Host.Or("")),
	}, nil
}

// RevokeGrant withdraws one grant.
func (d *apiDataSource) RevokeGrant(ctx context.Context, server, grantID string) error {
	d, err := d.on(ctx, server)
	if err != nil {
		return err
	}
	res, err := d.client.RevokeSecretGrant(ctx, apiclientgen.RevokeSecretGrantParams{
		ProjectId: d.projectID,
		GrantId:   grantID,
	})
	if err != nil {
		return err
	}
	return expectNoContent[apiclientgen.RevokeSecretGrantNoContent](res)
}

// DeleteSecret removes a secret and everything standing on it.
func (d *apiDataSource) DeleteSecret(ctx context.Context, server, secretID string) error {
	d, err := d.on(ctx, server)
	if err != nil {
		return err
	}
	res, err := d.client.DeleteSecret(ctx, apiclientgen.DeleteSecretParams{
		ProjectId: d.projectID,
		SecretId:  secretID,
	})
	if err != nil {
		return err
	}
	return expectNoContent[apiclientgen.DeleteSecretNoContent](res)
}
