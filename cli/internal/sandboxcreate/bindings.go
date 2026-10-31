package sandboxcreate

import (
	"context"
	"fmt"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/apiroute"
	"github.com/discobox-ai/discobox/cli/internal/credentialpolicy"
	"github.com/discobox-ai/discobox/cli/internal/secretenv"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	idpkg "github.com/discobox-ai/x/id"
)

// ParseBindings reads --bind and --delegate into the bindings a create makes
// for the new discobox (ADR 26-10-27-880 §5).
//
// A --bind is SECRET[:VAR][@ROUTE,...]: the secret, the variable the new
// discobox receives its sentinel in — the secret's name upper-snaked when none
// is named (secretenv.Name) — and routes that narrow the secret's for this
// binding, each a host (exact, `*.suffix` or `*`), optionally =blocked
// (ParseRoutes). A secret given by ID with no
// VAR is left unnamed here, for NameBindingVariables to name after the
// secret's name rather than its ID. A --delegate is
// SECRET[@ROUTE,...]: the new discobox may give the secret to the discoboxes
// it creates, and is delivered nothing itself. A secret is left as written —
// the server reads a name or an ID. One variable is filled once, and one
// secret delegated once, so a repeat is refused.
func ParseBindings(binds, delegates []string) ([]apimodel.SandboxBindingInput, error) {
	var out []apimodel.SandboxBindingInput
	vars := map[string]bool{}
	for _, value := range binds {
		named, routes, _ := strings.Cut(value, "@")
		var policies credentialpolicy.References
		if strings.Contains(value, ",policy=") {
			var tail string
			named, tail, _ = strings.Cut(value, ",")
			if strings.Contains(named, "@") {
				return nil, fmt.Errorf("--bind %q: policies and inline routes are mutually exclusive", value)
			}
			for _, part := range strings.Split(tail, ",") {
				var err error
				if path, ok := strings.CutPrefix(part, "policy="); ok {
					err = policies.Add(path)
				} else {
					err = policies.Argument(part)
				}
				if err != nil {
					return nil, fmt.Errorf("--bind %q: %w", value, err)
				}
			}
			routes = ""
		}
		secret, envVar, hasVar := strings.Cut(named, ":")
		secret, envVar = strings.TrimSpace(secret), strings.TrimSpace(envVar)
		byID := !hasVar && idpkg.IsGenerated(secret)
		if !hasVar && !byID {
			envVar = secretenv.Name(secret)
		}
		if secret == "" || (envVar == "" && !byID) || strings.ContainsAny(envVar, "=:@ ") {
			return nil, fmt.Errorf("--bind %q: want SECRET[:VAR][@ROUTE,...], such as github:GH_TOKEN@api.github.com", value)
		}
		if envVar != "" && vars[envVar] {
			return nil, fmt.Errorf("--bind %q: %s is already bound; a variable holds one credential", value, envVar)
		}
		vars[envVar] = true
		parsed, err := ParseRoutes(routes)
		if err != nil {
			return nil, fmt.Errorf("--bind %q: %w", value, err)
		}
		if len(policies) > 0 {
			resolved, err := policies.Resolve()
			if err != nil {
				return nil, fmt.Errorf("--bind %q: %w", value, err)
			}
			parsed = apiroute.Inputs(resolved)
		}
		binding := apimodel.SandboxBindingInput{Secret: secret, Routes: parsed}
		if envVar != "" {
			binding.EnvVar = apiclientgen.NewOptString(envVar)
		}
		out = append(out, binding)
	}
	delegated := map[string]bool{}
	for _, value := range delegates {
		secret, routes, _ := strings.Cut(value, "@")
		secret = strings.TrimSpace(secret)
		if secret == "" || strings.ContainsAny(secret, ": ") {
			return nil, fmt.Errorf("--delegate %q: want SECRET[@ROUTE,...], such as github@api.github.com", value)
		}
		if delegated[secret] {
			return nil, fmt.Errorf("--delegate %q: %s is already delegated", value, secret)
		}
		delegated[secret] = true
		parsed, err := ParseRoutes(routes)
		if err != nil {
			return nil, fmt.Errorf("--delegate %q: %w", value, err)
		}
		out = append(out, apimodel.SandboxBindingInput{
			Secret:   secret,
			Delegate: apiclientgen.NewOptBool(true),
			Use:      apiclientgen.NewOptBool(false),
			Routes:   parsed,
		})
	}
	return out, nil
}

// secretReader is the one call NameBindingVariables makes.
type secretReader interface {
	GetSecret(context.Context, apiclientgen.GetSecretParams) (apiclientgen.GetSecretRes, error)
}

// NameBindingVariables names the variable of each binding to use that
// ParseBindings left unnamed — a secret given by ID — after the secret's
// name, as `secret bind` does, so `--bind sec_…` lands in GITHUB rather than
// in the ID upper-snaked. A variable named twice that way is refused as
// ParseBindings refuses one.
func NameBindingVariables(ctx context.Context, client secretReader, projectID string, bindings []apimodel.SandboxBindingInput) error {
	vars := map[string]bool{}
	for _, b := range bindings {
		if env, ok := b.EnvVar.Get(); ok {
			vars[env] = true
		}
	}
	for i := range bindings {
		b := &bindings[i]
		if b.EnvVar.Set || b.Delegate.Or(false) {
			continue
		}
		res, err := client.GetSecret(ctx, apiclientgen.GetSecretParams{ProjectId: projectID, SecretId: b.Secret})
		if err != nil {
			return fmt.Errorf("--bind %s: %w", b.Secret, err)
		}
		secret, ok := res.(*apimodel.Secret)
		if !ok {
			return fmt.Errorf("--bind %s: %w", b.Secret, createResponseError(res))
		}
		env := secretenv.Name(secret.Name)
		if env == "" || vars[env] {
			return fmt.Errorf("--bind %s: %q is already bound or makes no variable; name one with %s:VAR", b.Secret, env, b.Secret)
		}
		vars[env] = true
		b.EnvVar = apiclientgen.NewOptString(env)
	}
	return nil
}

// ParseRoutes reads --bind's and --delegate's @ROUTE,... into what the API
// takes: comma-joined routes that each name one host — exact, `*.suffix` for
// the hosts beneath suffix, or `*` — optionally `=blocked`. The comma is the
// list's, so a route here is hosts-only; one with methods, paths or a
// protocol is made with `secret bind --route`. An empty value is no routes:
// the binding inherits its secret's.
func ParseRoutes(value string) ([]apimodel.SecretRouteInput, error) {
	var out []apimodel.SecretRouteInput
	for _, text := range strings.Split(value, ",") {
		if strings.TrimSpace(text) == "" {
			continue
		}
		route, err := secretroute.Parse(text)
		if err != nil {
			return nil, err
		}
		if len(route.Methods) > 0 || len(route.Paths) > 0 || route.GraphQL != nil || route.MCP != nil {
			return nil, fmt.Errorf("route %q: a route here is a host, `*.suffix` or `*`, optionally =blocked; bind one with methods, paths or a protocol with `discobox secret bind --route`", strings.TrimSpace(text))
		}
		out = append(out, apiroute.Input(route))
	}
	return out, nil
}
