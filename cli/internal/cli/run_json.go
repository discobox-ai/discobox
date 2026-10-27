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

// runJSONRequest is `discobox run --from-json`: the command's request as one JSON
// object on stdin, for a caller — an agent, above all — that would otherwise
// have to quote a prompt through a shell (ADR 26-09-24-630 §4).
// Its fields are the run flags', and mean what they mean; -d and --raw have
// none, because a JSON run creates, prints, and returns.
type runJSONRequest struct {
	// Source is SOURCE, required unless NoSource is set.
	Source  string         `json:"source"`
	Prompt  string         `json:"prompt"`
	Harness string         `json:"harness"`
	Grants  []runJSONGrant `json:"grants"`
	// DiscoboxAPI is --discobox-api.
	DiscoboxAPI bool     `json:"discoboxApi"`
	Env         []string `json:"env"`
	Secrets     []string `json:"secrets"`
	Include     []string `json:"include"`
	// Skills and UserSkills are --skills and --user-skills. Left out, each
	// takes what client.yaml says; "skills": [] is none.
	Skills     *[]string `json:"skills"`
	UserSkills *bool     `json:"userSkills"`
	// NoProjectSkills is --no-project-skills.
	NoProjectSkills bool `json:"noProjectSkills"`
	// Pool is --pool.
	Pool string `json:"pool"`
	// NoSource, IncludeDirty and DeclaredSources are --no-source,
	// --include-dirty and --declared-sources. IncludeDirty left out leaves
	// the uncommitted work behind, as the flag left out does.
	NoSource        bool  `json:"noSource"`
	IncludeDirty    *bool `json:"includeDirty"`
	DeclaredSources *bool `json:"declaredSources"`
}

// runJSONGrant is one --grant: a project secret, the variable the new
// discobox receives its sentinel in, and the hosts it may be sent to.
type runJSONGrant struct {
	Secret string   `json:"secret"`
	EnvVar string   `json:"envVar"`
	Hosts  []string `json:"hosts"`
}

// readJSONRequest settles opts from the request on stdin. The command line may
// say nothing else a run takes: a flag beside --from-json would be a second answer to
// a question the request already answers, or an answer the request left out on
// purpose.
func (opts *runCommandOptions) readJSONRequest(cmd *cobra.Command, args []string) error {
	var given []string
	opts.flags.VisitAll(func(flag *pflag.Flag) {
		if flag.Changed && flag.Name != "from-json" {
			given = append(given, "--"+flag.Name)
		}
	})
	if len(given) > 0 {
		return fmt.Errorf("--from-json reads the whole request from stdin; put %s in it instead", strings.Join(given, ", "))
	}
	if len(args) > 0 {
		return errors.New("--from-json reads the request from stdin; put SOURCE in its \"source\" and the prompt in its \"prompt\"")
	}
	var req runJSONRequest
	decoder := json.NewDecoder(cmd.InOrStdin())
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return fmt.Errorf("--from-json: read the request from stdin: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("--from-json: stdin holds more than one request")
	}
	grants, err := req.grants()
	if err != nil {
		return err
	}
	opts.grants = grants
	opts.discoboxAPI = req.DiscoboxAPI
	if req.Prompt != "" {
		opts.promptFlag = []string{req.Prompt}
	}
	opts.prompt.Harness = req.Harness
	opts.prompt.Env = req.Env
	opts.prompt.Secret = req.Secrets
	opts.prompt.Include = req.Include
	if req.Skills != nil {
		opts.skillDirs = append([]string{}, *req.Skills...)
	}
	opts.userSkills = req.UserSkills
	opts.noProjectSkills = req.NoProjectSkills
	opts.pool = req.Pool
	opts.noSource = req.NoSource
	if req.Source != "" {
		if req.NoSource {
			return errors.New("--from-json: \"source\" and \"noSource\" say opposite things; give one")
		}
		source, err := runSourceArgument(req.Source)
		if err != nil {
			return err
		}
		opts.source = source
	}
	opts.prompt.IncludeDirty = sandboxcreate.IncludeDirtyNever
	if req.IncludeDirty != nil && *req.IncludeDirty {
		opts.prompt.IncludeDirty = sandboxcreate.IncludeDirtyAlways
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
	seen := map[string]bool{}
	for i, in := range req.Grants {
		secret, envVar := strings.TrimSpace(in.Secret), strings.TrimSpace(in.EnvVar)
		if secret == "" || envVar == "" {
			return nil, fmt.Errorf(`--from-json: grants[%d]: want {"secret": SECRET, "envVar": ENV_VAR}, with "hosts" to narrow where it may be sent`, i)
		}
		if seen[envVar] {
			return nil, fmt.Errorf("--from-json: grants[%d]: %s is already given; a variable holds one credential", i, envVar)
		}
		seen[envVar] = true
		var grant apimodel.SandboxGrant
		grant.SetSecretId(apiclientgen.NewOptString(secret))
		grant.SetEnvVar(apiclientgen.NewOptString(envVar))
		grant.SetHosts(in.Hosts)
		grants = append(grants, grant)
	}
	return grants, nil
}
