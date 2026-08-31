package cli

import (
	"context"
	"strings"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// The window's half of the credential inbox: the requests waiting on a person,
// the secrets one of them can be answered with, and the two answers.
//
// Everything here is the same API the `discobox secret` commands use. The
// window adds no shortcut of its own — an approval is the same act however it
// is made, and a second path to it that skipped a check would be the one worth
// exploiting.

// CredentialRequests returns the project's pending requests, newest first.
func (d *apiDataSource) CredentialRequests(ctx context.Context) ([]tui.CredentialRequest, error) {
	res, err := d.client.ListSecretRequests(ctx, apiclientgen.ListSecretRequestsParams{
		ProjectId: d.projectID,
		Status:    apiclientgen.NewOptListSecretRequestsStatus(apiclientgen.ListSecretRequestsStatusPending),
	})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListSecretRequestsBody](res)
	if err != nil {
		return nil, err
	}
	requests := sortedByRecency(body.GetSecretRequests(), func(r apimodel.SecretRequest) time.Time { return r.CreatedAt })
	out := make([]tui.CredentialRequest, 0, len(requests))
	for _, r := range requests {
		out = append(out, toTUICredentialRequest(r))
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

// Secrets returns the project's secrets, without values.
func (d *apiDataSource) Secrets(ctx context.Context) ([]tui.Secret, error) {
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
			ID:      s.ID,
			Name:    s.Name,
			Type:    string(s.Type),
			Host:    strings.TrimSpace(s.Host.Or("")),
			MaxTTL:  time.Duration(s.MaxGrantTTLSeconds) * time.Second,
			Created: s.CreatedAt,
			Updated: s.UpdatedAt,
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
func (d *apiDataSource) CreateSecret(ctx context.Context, secret tui.NewSecret) (tui.Secret, error) {
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
	body.Value = apimodel.SecretValue{Token: apiclientgen.NewOptString(secret.Value)}
	// What makes it an oauth credential rather than a token: the material the
	// control plane spends to renew the access token.
	if secret.RefreshToken != "" {
		body.Value.SetRefreshToken(apiclientgen.NewOptString(secret.RefreshToken))
	}
	if secret.TokenURL != "" {
		body.Value.SetTokenUrl(apiclientgen.NewOptString(secret.TokenURL))
	}
	if secret.ClientID != "" {
		body.Value.SetClientId(apiclientgen.NewOptString(secret.ClientID))
	}
	if len(secret.Scopes) > 0 {
		body.Value.SetScopes(apiclientgen.NewOptNilStringArray(secret.Scopes))
	}

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

// SetSecretHost binds a secret to a host, or releases it with an empty one.
func (d *apiDataSource) SetSecretHost(ctx context.Context, secretID, host string) error {
	body := &apimodel.UpdateSecretBody{}
	// Set, even when empty: an unset field changes nothing, so releasing a
	// binding has to be said rather than left out.
	body.SetHost(apiclientgen.NewOptString(strings.TrimSpace(host)))
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

// SetSecretMaxGrantTTL sets the longest a grant on the secret may live; zero
// lifts the limit.
func (d *apiDataSource) SetSecretMaxGrantTTL(ctx context.Context, secretID string, seconds int64) error {
	body := &apimodel.UpdateSecretBody{}
	// Set, even at zero: zero is "no limit" rather than "unchanged".
	body.SetMaxGrantTTLSeconds(apiclientgen.NewOptInt64(seconds))
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
func (d *apiDataSource) ApproveCredentialRequest(ctx context.Context, approval tui.Approval) error {
	body := &apimodel.ApproveSecretRequestBody{SecretId: approval.SecretID}
	if approval.TTLSeconds > 0 {
		body.SetGrantTTLSeconds(apiclientgen.NewOptInt64(approval.TTLSeconds))
	}
	res, err := d.client.ApproveSecretRequest(ctx, body, apiclientgen.ApproveSecretRequestParams{
		ProjectId: d.projectID,
		RequestId: approval.RequestID,
	})
	if err != nil {
		return err
	}
	_, err = expectResponse[apimodel.SecretRequest](res)
	return err
}

// DenyCredentialRequest answers a request no.
func (d *apiDataSource) DenyCredentialRequest(ctx context.Context, requestID string) error {
	res, err := d.client.DenySecretRequest(ctx, apiclientgen.DenySecretRequestParams{
		ProjectId: d.projectID,
		RequestId: requestID,
	})
	if err != nil {
		return err
	}
	return expectNoContent[apiclientgen.DenySecretRequestNoContent](res)
}

// Grants lists the standing grants on a secret, or on the whole project.
func (d *apiDataSource) Grants(ctx context.Context, secretID string) ([]tui.Grant, error) {
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
func (d *apiDataSource) CreateGrant(ctx context.Context, grant tui.NewGrant) (tui.Grant, error) {
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
func (d *apiDataSource) RevokeGrant(ctx context.Context, grantID string) error {
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
func (d *apiDataSource) DeleteSecret(ctx context.Context, secretID string) error {
	res, err := d.client.DeleteSecret(ctx, apiclientgen.DeleteSecretParams{
		ProjectId: d.projectID,
		SecretId:  secretID,
	})
	if err != nil {
		return err
	}
	return expectNoContent[apiclientgen.DeleteSecretNoContent](res)
}
