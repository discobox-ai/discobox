package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// This file is the exchange secret's half of renewal (ADR 26-10-08-452): the
// fields a person stores are traded for a short-lived token by the recipe the
// secret carries. recipe.go reads the recipe; renew.go decides when.

// exchangeUnknownLifetime is how long a token is trusted when neither the
// endpoint's answer nor the token itself says how long it lasts. Past the
// renewal skew, so it is renewed a few minutes before this, not every resolve.
const exchangeUnknownLifetime = 15 * time.Minute

// exchangeToken trades a value's stored fields for a token, returning the
// value with the token and its expiry in place. The stored fields are not
// spent, so unlike an OAuth refresh an exchange may be repeated.
func exchangeToken(ctx context.Context, secret *model.Secret, val *model.SecretValue) (*model.SecretValue, error) {
	if secret.ExchangeRecipe == nil {
		return nil, fmt.Errorf("exchange secret %s has no recipe", secret.ID)
	}
	req, err := recipeRequest(ctx, secret.ExchangeRecipe, val.Exchange)
	if err != nil {
		return nil, err
	}
	if err := refuseInternalTarget(ctx, req.URL.Hostname()); err != nil {
		return nil, err
	}
	resp, err := exchangeClient(req.URL).Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchange request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if refusedStatus(resp.StatusCode) {
		// The endpoint answered, and its answer is about what is stored: the
		// key is wrong, revoked, or of an account that is gone (ADR 0132 §3).
		return nil, fmt.Errorf("%w: %s", errRenewalRefused, upstreamAnswer(resp.Status, body))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("exchange failed: %s", upstreamAnswer(resp.Status, body))
	}
	token, expiry, err := recipeToken(secret.ExchangeRecipe, body)
	if err != nil {
		return nil, err
	}
	if expiry.IsZero() {
		// Neither the answer nor the token says. Unknown would read as
		// "renew now" on every resolve — an upstream call and a write per
		// request — so the token is trusted for a while instead, and an
		// upstream refusing it sooner renews it at once (ADR 0132 §3).
		expiry = time.Now().UTC().Add(exchangeUnknownLifetime)
	}
	exchanged := *val
	exchanged.Token = token
	exchanged.AccessTokenExpiresAt = expiry.UnixMilli()
	return &exchanged, nil
}

// exchangeRecipe is the recipe a create or update gives, checked: only a
// person names where a key is sent, only an exchange secret has one, and its
// URL must sit inside the secret's binding. Nil when none was given.
func exchangeRecipe(ctx context.Context, secretType, host string, given apigen.OptExchangeRecipe) (*model.ExchangeRecipe, error) {
	in, ok := given.Get()
	if !ok {
		return nil, nil
	}
	if principal, ok := auth.PrincipalFromContext(ctx); ok && principal.Type == auth.PrincipalTypeSandbox {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "a discobox may not say where a key is sent: an exchange recipe is a person's to give")
	}
	if secretType != model.SecretTypeExchange {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "only an exchange secret has an exchange recipe")
	}
	recipe := &model.ExchangeRecipe{
		URL:           strings.TrimSpace(in.URL),
		Form:          in.Form.Or(false),
		Fields:        in.Fields,
		TokenPath:     strings.TrimSpace(in.TokenPath),
		ExpiresAtPath: strings.TrimSpace(in.ExpiresAtPath.Or("")),
		ExpiresInPath: strings.TrimSpace(in.ExpiresInPath.Or("")),
	}
	if body, ok := in.Body.Get(); ok {
		recipe.Body = body
	}
	if header, ok := in.Header.Get(); ok {
		recipe.Header = header
	}
	if err := checkRecipe(recipe, host); err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	if u, err := url.Parse(recipe.URL); err == nil {
		if err := refuseInternalTarget(ctx, u.Hostname()); err != nil {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
		}
	}
	return recipe, nil
}

// internalAddress reports whether an exchange may not be sent to ip: one only
// the control plane's own network position reaches — loopback, private,
// carrier-grade NAT, link-local, unspecified, or multicast. A recipe is
// somebody's to write, and the server POSTing where they cannot, then reading
// them the answer, is a reach they never had. It is a package var so tests can
// exchange against an httptest server on loopback.
var internalAddress = func(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || sharedAddressSpace.Contains(ip)
}

// sharedAddressSpace is RFC 6598's carrier-grade NAT range, which netip does
// not count as private and which cloud providers use internally.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// refuseInternalTarget refuses a host that is, or resolves to, an internal
// address. Resolving here catches a name pointed inward when a proxy does the
// dialing; exchangeClient checks the address it actually dials, so a name
// that answers differently a second time is caught too.
func refuseInternalTarget(ctx context.Context, host string) error {
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if internalAddress(ip) {
			what := host
			if ip.Unmap().String() != host {
				what = host + " resolves to " + ip.Unmap().String() + ", which"
			}
			return fmt.Errorf("%s is an internal address: an exchange is only sent to a public endpoint", what)
		}
	}
	return nil
}

// exchangeClient is tokenHTTPClient whose dial to target itself checks the
// address it connects to. A dial to anything else is the configured proxy,
// which may well sit on loopback, and is left alone.
func exchangeClient(target *url.URL) *http.Client {
	base, _ := tokenHTTPClient.Transport.(*http.Transport)
	if base == nil {
		base, _ = http.DefaultTransport.(*http.Transport)
	}
	transport := base.Clone()
	port := target.Port()
	if port == "" {
		port = "443"
	}
	authority := net.JoinHostPort(target.Hostname(), port)
	plain := transport.DialContext
	if plain == nil {
		plain = (&net.Dialer{Timeout: tokenRenewTimeout}).DialContext
	}
	checked := &net.Dialer{Timeout: tokenRenewTimeout, Control: func(_, address string, _ syscall.RawConn) error {
		addrPort, err := netip.ParseAddrPort(address)
		if err != nil {
			return err
		}
		if internalAddress(addrPort.Addr()) {
			return fmt.Errorf("%s resolved to %s, an internal address: an exchange is only sent to a public endpoint", target.Hostname(), addrPort.Addr().Unmap())
		}
		return nil
	}}
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == authority {
			return checked.DialContext(ctx, network, addr)
		}
		return plain(ctx, network, addr)
	}
	client := *tokenHTTPClient
	client.Transport = transport
	return &client
}

// firstExchange is the value an exchange secret is stored with: the fields
// given, checked against its recipe, and the token they were just exchanged
// for (ADR 26-10-08-452 §3). A key the endpoint refuses is refused here, to the
// person storing it, rather than to a discobox an hour later. A token given
// with the fields is the server's to obtain, and is dropped.
func firstExchange(ctx context.Context, sec *model.Secret, given apigen.SecretValue) ([]byte, error) {
	recipe := sec.ExchangeRecipe
	if recipe == nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			"an exchange secret needs an exchange recipe: where its fields are exchanged, and where the token is in the answer")
	}
	fields := map[string]string{}
	if stored, ok := given.Exchange.Get(); ok {
		for name, value := range stored {
			fields[strings.TrimSpace(name)] = value
		}
	}
	for name := range fields {
		if !slices.Contains(recipe.Fields, name) {
			return nil, apperrors.NewStatusError(http.StatusBadRequest,
				fmt.Sprintf("its recipe stores %s, not %q", strings.Join(recipe.Fields, ", "), name))
		}
	}
	if missing := missingFields(recipe, fields); len(missing) > 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("its recipe needs %s in the value's exchange fields", strings.Join(missing, ", ")))
	}
	exchanged, err := exchangeToken(ctx, sec, &model.SecretValue{Exchange: fields})
	if err != nil {
		if errors.Is(err, errRenewalRefused) {
			// Its answer, without the renewal vocabulary: nothing is being
			// renewed yet.
			answer := strings.TrimPrefix(err.Error(), errRenewalRefused.Error()+": ")
			return nil, apperrors.NewStatusError(http.StatusBadRequest,
				fmt.Sprintf("%s refused it, so it was not stored: %s", recipeHost(recipe), answer))
		}
		return nil, apperrors.NewStatusError(http.StatusBadGateway,
			fmt.Sprintf("could not exchange it at %s: %v", recipeHost(recipe), err))
	}
	//nolint:gosec // Secret values are intentionally marshaled before store encryption.
	return json.Marshal(exchanged)
}

// describeExchange fills in what an exchange credential is, for a caller that
// may not see what it holds: its recipe, and when the token goes stale. Like
// describeOAuth, a value that cannot be read leaves that half empty rather
// than failing the read.
func (s *Service) describeExchange(ctx context.Context, secret *model.Secret) {
	if secret == nil || secret.Type != model.SecretTypeExchange {
		return
	}
	summary := &model.SecretExchange{Recipe: secret.ExchangeRecipe}
	if value, err := s.store.OpenSecretValue(ctx, secret); err == nil && value != nil {
		summary.TokenExpiresAt = value.AccessTokenExpiresAt
	}
	secret.Exchange = summary
}

// recipeHost is the host a recipe's endpoint is at, for an error a person reads.
func recipeHost(recipe *model.ExchangeRecipe) string {
	if u, err := url.Parse(recipe.URL); err == nil && u.Host != "" {
		return u.Host
	}
	return recipe.URL
}
