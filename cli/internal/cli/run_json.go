package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// runJSONRequest is `discobox new --json`: the command's request as one JSON
// object on stdin, for a caller — an agent, above all — that would otherwise
// have to quote a prompt and each use's sentence through a shell (ADR 0149 §4).
// Its fields are the run flags', and mean what they mean; -d and --raw have
// none, because a JSON run creates, prints, and returns.
type runJSONRequest struct {
	Prompt  string         `json:"prompt"`
	Harness string         `json:"harness"`
	Grants  []runJSONGrant `json:"grants"`
	Env     []string       `json:"env"`
	Secrets []string       `json:"secrets"`
	Include []string       `json:"include"`
	// NoSource, IncludeDirty and DeclaredSources are --no-source,
	// --include-dirty and --declared-sources. IncludeDirty left out is auto,
	// which with nobody to ask carries the uncommitted work.
	NoSource        bool  `json:"noSource"`
	IncludeDirty    *bool `json:"includeDirty"`
	DeclaredSources *bool `json:"declaredSources"`
}

// runJSONGrant is one --grant, spelled the way `discobox-access request --json`
// asks for a credential: a well-known credential by its ID, or a project secret
// and the variable its agent receives it in, with the sentences it may be used
// for.
type runJSONGrant struct {
	ID     string       `json:"id"`
	Secret string       `json:"secret"`
	EnvVar string       `json:"envVar"`
	Host   string       `json:"host"`
	Uses   []runJSONUse `json:"uses"`
}

type runJSONUse struct {
	Description string `json:"description"`
}

// readJSONRequest settles opts from the request on stdin. The command line may
// say nothing else a run takes: a flag beside --json would be a second answer to
// a question the request already answers, or an answer the request left out on
// purpose.
func (opts *runCommandOptions) readJSONRequest(cmd *cobra.Command, args []string) error {
	var given []string
	opts.flags.VisitAll(func(flag *pflag.Flag) {
		if flag.Changed && flag.Name != "json" {
			given = append(given, "--"+flag.Name)
		}
	})
	if len(given) > 0 {
		return fmt.Errorf("--json reads the whole request from stdin; put %s in it instead", strings.Join(given, ", "))
	}
	if len(args) > 0 {
		return errors.New("--json reads the prompt from stdin; put it in the request's \"prompt\"")
	}
	var req runJSONRequest
	decoder := json.NewDecoder(cmd.InOrStdin())
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return fmt.Errorf("--json: read the request from stdin: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("--json: stdin holds more than one request")
	}
	grants, err := req.grants()
	if err != nil {
		return err
	}
	opts.grants = grants
	if req.Prompt != "" {
		opts.promptFlag = []string{req.Prompt}
	}
	opts.prompt.Harness = req.Harness
	opts.prompt.Env = req.Env
	opts.prompt.Secret = req.Secrets
	opts.prompt.Include = req.Include
	opts.noSource = req.NoSource
	if req.IncludeDirty != nil {
		opts.prompt.IncludeDirty = sandboxcreate.IncludeDirtyNever
		if *req.IncludeDirty {
			opts.prompt.IncludeDirty = sandboxcreate.IncludeDirtyAlways
		}
	}
	if req.DeclaredSources != nil {
		opts.declaredSources = *req.DeclaredSources
	}
	opts.detach = true
	return nil
}

// grants are the request's grants in the form the create takes, checked for
// the shape --grant would have refused.
func (req runJSONRequest) grants() ([]apimodel.SandboxGrant, error) {
	grants := make([]apimodel.SandboxGrant, 0, len(req.Grants))
	for i, in := range req.Grants {
		id, secret, envVar := strings.TrimSpace(in.ID), strings.TrimSpace(in.Secret), strings.TrimSpace(in.EnvVar)
		var grant apimodel.SandboxGrant
		switch {
		case id != "" && secret == "" && envVar == "":
			grant.SetWellKnownId(apiclientgen.NewOptString(id))
		case id == "" && secret != "" && envVar != "":
			grant.SetSecretId(apiclientgen.NewOptString(secret))
			grant.SetEnvVar(apiclientgen.NewOptString(envVar))
		default:
			return nil, fmt.Errorf(`--json: grants[%d]: want {"id": ID} for a well-known credential, such as com.github.api, or {"secret": SECRET, "envVar": ENV_VAR}, not both`, i)
		}
		if host := strings.TrimSpace(in.Host); host != "" {
			grant.SetHost(apiclientgen.NewOptString(host))
		}
		for _, use := range in.Uses {
			if description := strings.TrimSpace(use.Description); description != "" {
				grant.Uses = append(grant.Uses, apimodel.SecretUse{Description: description})
			}
		}
		if len(grant.Uses) == 0 {
			return nil, fmt.Errorf(`--json: grants[%d]: want at least one use, as "uses": [{"description": "what it is for"}]`, i)
		}
		grants = append(grants, grant)
	}
	return grants, nil
}
