package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

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
	Source   string           `json:"source"`
	Prompt   string           `json:"prompt"`
	Harness  string           `json:"harness"`
	Bindings []runJSONBinding `json:"bindings"`
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

// runJSONBinding is one --bind or --delegate: a project secret, the variable
// the new discobox receives its sentinel in (for --bind; the secret's name
// upper-snaked when left out), routes that narrow
// where it may be sent, each as --to writes one, and whether the new
// discobox may give it on (for --delegate).
type runJSONBinding struct {
	Secret   string   `json:"secret"`
	EnvVar   string   `json:"envVar"`
	Routes   []string `json:"routes"`
	Delegate bool     `json:"delegate"`
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
	bindings, err := req.bindings()
	if err != nil {
		return err
	}
	opts.bindings = bindings
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

// bindings are the request's bindings in the form the create takes, checked
// as --bind and --delegate check theirs.
func (req runJSONRequest) bindings() ([]apimodel.SandboxBindingInput, error) {
	var binds, delegates []string
	for i, in := range req.Bindings {
		secret, envVar := strings.TrimSpace(in.Secret), strings.TrimSpace(in.EnvVar)
		routes := strings.Join(in.Routes, ",")
		switch {
		case secret == "":
			return nil, fmt.Errorf(`--from-json: bindings[%d]: want {"secret": SECRET}, with "envVar" to name its variable, "routes" to narrow where it may be sent, or "delegate": true to let the new discobox give it on`, i)
		case in.Delegate && envVar != "":
			return nil, fmt.Errorf("--from-json: bindings[%d]: a delegation is delivered to nothing, so it names no envVar", i)
		case in.Delegate:
			if routes != "" {
				secret += "@" + routes
			}
			delegates = append(delegates, secret)
		default:
			value := secret
			if envVar != "" {
				value += ":" + envVar
			}
			if routes != "" {
				value += "@" + routes
			}
			binds = append(binds, value)
		}
	}
	bindings, err := sandboxcreate.ParseBindings(binds, delegates)
	if err != nil {
		return nil, fmt.Errorf("--from-json: %w", err)
	}
	return bindings, nil
}
