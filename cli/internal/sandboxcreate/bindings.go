package sandboxcreate

import (
	"context"
	"fmt"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
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
// binding, each HOST[/PATH][=blocked] or `*`. A secret given by ID with no
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

// ParseRoutes reads comma-joined routes, each as secretroute.Parse reads one,
// into what the API takes. An empty value is no routes: the binding inherits
// its secret's.
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
		out = append(out, RouteInput(route))
	}
	return out, nil
}

// RouteInput is a parsed route as the API takes it.
func RouteInput(route secretroute.Route) apimodel.SecretRouteInput {
	in := apimodel.SecretRouteInput{Host: route.Host}
	if route.Path != "" {
		in.Path = apiclientgen.NewOptString(route.Path)
	}
	if route.Blocked {
		in.Blocked = apiclientgen.NewOptBool(true)
	}
	return in
}
