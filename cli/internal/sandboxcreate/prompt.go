package sandboxcreate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/randomname"
)

// PromptOptions describes the client-side inputs used to create a sandbox for
// a prompt. Frontends should populate this type and let this package normalize
// and resolve the request consistently.
type PromptOptions struct {
	Source  string
	Ref     string
	Prompt  []string
	Env     []string
	Secret  []string
	Harness string
	// IncludeDirty decides what happens to uncommitted work in a local source.
	// The zero value is "auto": ask through ConfirmIncludeDirty when there is
	// one, and otherwise include it.
	IncludeDirty IncludeDirty
	// ConfirmIncludeDirty answers the "auto" question for the frontend. Leave it
	// nil when the frontend cannot ask.
	ConfirmIncludeDirty ConfirmIncludeDirtyFunc
}

// ParsePromptOptions adds positional prompt arguments and normalizes the source.
func ParsePromptOptions(opts PromptOptions, args []string) (PromptOptions, error) {
	opts.Prompt = append([]string(nil), args...)
	return normalizePromptOptions(opts)
}

func normalizePromptOptions(opts PromptOptions) (PromptOptions, error) {
	if strings.TrimSpace(opts.Source) == "" {
		return PromptOptions{}, errors.New("source directory or Git repository is required")
	}
	if opts.Ref == "" {
		source, ref, ok := splitRunSourceRef(opts.Source)
		if ok {
			opts.Source = source
			opts.Ref = ref
		}
	}
	if opts.IncludeDirty == "" {
		opts.IncludeDirty = IncludeDirtyAuto
	}
	if err := opts.IncludeDirty.Set(string(opts.IncludeDirty)); err != nil {
		return PromptOptions{}, fmt.Errorf("--include-dirty: %w", err)
	}
	return opts, nil
}

// BuildPromptSandboxBody resolves all client-side prompt inputs into the
// control-plane create request, including Git snapshots and local user identity.
func BuildPromptSandboxBody(ctx context.Context, opts PromptOptions) (*apimodel.CreateSandboxBody, error) {
	opts, err := normalizePromptOptions(opts)
	if err != nil {
		return nil, err
	}
	name, err := randomname.Generate()
	if err != nil {
		return nil, fmt.Errorf("generate sandbox name: %w", err)
	}
	body := &apimodel.CreateSandboxBody{Config: apimodel.SandboxCreateConfig{Name: name}}
	if len(opts.Prompt) > 0 {
		body.Config.SetPrompt(append([]string(nil), opts.Prompt...))
	}
	if strings.TrimSpace(opts.Harness) != "" {
		body.SetHarnessName(optionalString(opts.Harness))
	}
	env, secrets, err := EnvAndSecretsFromOptions(opts.Env, opts.Secret)
	if err != nil {
		return nil, err
	}
	if len(env) > 0 {
		body.Config.SetEnv(apiclientgen.NewOptSandboxCreateConfigEnv(apiclientgen.SandboxCreateConfigEnv(env)))
	}
	if len(secrets) > 0 {
		body.Config.SetSecrets(secrets)
	}
	userIdentity, _, err := resolveRunUserIdentity()
	if err != nil {
		return nil, err
	}
	sourceArg := opts.Source
	if opts.Ref != "" {
		sourceArg += "@" + opts.Ref
	}
	source, err := resolveRunSource(ctx, sourceArg, runSourceOptions{
		IncludeDirty: opts.IncludeDirty,
		Confirm:      opts.ConfirmIncludeDirty,
	})
	if err != nil {
		return nil, err
	}
	apiSource, err := source.apiGitSource()
	if err != nil {
		return nil, err
	}
	body.Config.SetSource(apiclientgen.NewOptGitSource(*apiSource))
	resolvedOrigin, err := ResolveOrigin(ctx, sourceArg)
	if err != nil {
		return nil, err
	}
	body.SetOrigin(apiclientgen.NewOptOrigin(resolvedOrigin))
	userIdentity.setCreateSandboxUser(body)
	return body, nil
}

type promptSandboxCreator interface {
	CreateSandbox(context.Context, *apimodel.CreateSandboxBody, apiclientgen.CreateSandboxParams) (apiclientgen.CreateSandboxRes, error)
}

// CreatePromptSandbox builds, submits, and decodes a prompt sandbox request.
func CreatePromptSandbox(ctx context.Context, client promptSandboxCreator, projectID string, opts PromptOptions) (*apimodel.Sandbox, error) {
	body, err := BuildPromptSandboxBody(ctx, opts)
	if err != nil {
		return nil, err
	}
	res, err := client.CreateSandbox(ctx, body, apiclientgen.CreateSandboxParams{ProjectId: projectID})
	if err != nil {
		return nil, err
	}
	if sandbox, ok := res.(*apimodel.Sandbox); ok {
		return sandbox, nil
	}
	return nil, createResponseError(res)
}

func createResponseError(res any) error {
	if problem, ok := res.(*apiclientgen.ErrorModelStatusCode); ok {
		status := problem.StatusCode
		title := problem.Response.Title.Value
		detail := problem.Response.Detail.Value
		switch {
		case title != "" && detail != "":
			return fmt.Errorf("request failed: %d %s: %s", status, title, detail)
		case title != "":
			return fmt.Errorf("request failed: %d %s", status, title)
		case detail != "":
			return fmt.Errorf("request failed: %d %s", status, detail)
		case status != 0:
			return fmt.Errorf("request failed: %d %s", status, http.StatusText(status))
		}
	}
	if problem, ok := res.(*apiclientgen.ErrorResponseStatusCode); ok {
		status := problem.StatusCode
		if problem.Response.Error != "" {
			return fmt.Errorf("request failed: %d %s", status, problem.Response.Error)
		}
		if status != 0 {
			return fmt.Errorf("request failed: %d %s", status, http.StatusText(status))
		}
	}
	return fmt.Errorf("unexpected response type %T", res)
}

func optionalString(value string) apiclientgen.OptString {
	if strings.TrimSpace(value) == "" {
		return apiclientgen.OptString{}
	}
	return apiclientgen.NewOptString(value)
}
