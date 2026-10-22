package sandboxcreate

import (
	"fmt"
	"slices"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/apipolicy"
	"github.com/discobox-ai/discobox/pkg/policy"
)

// ParseGrants reads --grant values into the grants a create gives the new
// discobox. A value is SECRET[@HOST]:ENV_VAR=USE, or ID[@HOST]=USE for a
// well-known credential, whose ID carries its secret and variable; the first
// has a ":" before the "=" and the second does not. HOST may be several,
// joined by commas, for a credential sent to more than one site (ADR
// 26-10-02-393). Values naming the same
// credential, host, and variable are one grant with several uses, in the order
// given. A secret is left as written, for the caller to resolve to an ID.
//
// Each grant carries a policy (#312), from --grant-statement and
// --grant-define values: CREDENTIAL=SENTENCE and CREDENTIAL=WORD=DEFINITION,
// CREDENTIAL being what a --grant names before its "=" and matched as the
// --grant values are, so one spelled with its hosts in another order is the
// same grant. A grant with no statement, or a statement or definition for no
// grant, is refused, and so is a policy that does not parse or validate, with
// what to fix, before anything is sent.
func ParseGrants(values, statements, definitions []string) ([]apimodel.SandboxGrant, error) {
	var grants []apimodel.SandboxGrant
	var keys []string
	index := map[string]int{}
	for _, value := range values {
		spec, use, ok := strings.Cut(value, "=")
		use = strings.TrimSpace(use)
		key, grant, valid := grantSpec(spec)
		if !ok || !valid || use == "" {
			return nil, fmt.Errorf("--grant %q: want SECRET[@HOST]:ENV_VAR=USE, such as github@github.com:GH_TOKEN=\"push a branch to org/repo\", or ID[@HOST]=USE for a well-known credential, such as com.github.api=\"push a branch to org/repo\"", value)
		}
		i, seen := index[key]
		if !seen {
			grants = append(grants, grant)
			keys = append(keys, key)
			i = len(grants) - 1
			index[key] = i
		}
		grants[i].Uses = append(grants[i].Uses, apimodel.SecretUse{Description: use})
	}
	written := make([][]string, len(grants))
	defined := make([]policy.Definitions, len(grants))
	for _, flagged := range []struct {
		flag   string
		values []string
	}{{"--grant-statement", statements}, {"--grant-define", definitions}} {
		for _, value := range flagged.values {
			spec, said, _ := strings.Cut(value, "=")
			key, _, valid := grantSpec(spec)
			i, seen := index[key]
			if !valid || !seen {
				return nil, fmt.Errorf("%s %q names no --grant: it is CREDENTIAL=..., CREDENTIAL being what a --grant names before its \"=\"", flagged.flag, value)
			}
			if flagged.flag == "--grant-statement" {
				written[i] = append(written[i], said)
				continue
			}
			if defined[i] == nil {
				defined[i] = policy.Definitions{}
			}
			if err := defined[i].Set(said); err != nil {
				return nil, fmt.Errorf("%s %q: %w", flagged.flag, value, err)
			}
		}
	}
	for i, key := range keys {
		asked, err := policy.Written(written[i], defined[i])
		if err != nil {
			return nil, fmt.Errorf("--grant %s: %w\na statement is given with --grant-statement %s=SENTENCE, and a word that is not a default with --grant-define %s=WORD=DEFINITION", key, err, key, key)
		}
		if asked == nil {
			return nil, fmt.Errorf("--grant %s carries a policy: give each sentence of what it may do with --grant-statement %s=SENTENCE, and each word that is not a default with --grant-define %s=WORD=DEFINITION", key, key, key)
		}
		sent, err := apipolicy.To(*asked)
		if err != nil {
			return nil, err
		}
		grants[i].SetPolicy(apiclientgen.NewOptAccessPolicy(sent))
	}
	return grants, nil
}

// grantSpec reads what a --grant names before its "=": the grant it is, and
// the key it is one grant under, the hosts as a set, as the server reads
// them. It is not valid without a credential, or with a ":" and no variable.
func grantSpec(spec string) (string, apimodel.SandboxGrant, bool) {
	target, envVar, named := strings.Cut(spec, ":")
	credential, host, _ := strings.Cut(target, "@")
	credential, host, envVar = strings.TrimSpace(credential), strings.TrimSpace(host), strings.TrimSpace(envVar)
	var grant apimodel.SandboxGrant
	if credential == "" || (named && envVar == "") {
		return "", grant, false
	}
	hosts := grantHosts(host)
	if named {
		grant.SetSecretId(apiclientgen.NewOptString(credential))
		grant.SetEnvVar(apiclientgen.NewOptString(envVar))
	} else {
		grant.SetWellKnownId(apiclientgen.NewOptString(credential))
	}
	// None named leaves the list out, which takes the secret's host, or the
	// well-known credential's.
	grant.SetHosts(hosts)
	key := credential
	if len(hosts) > 0 {
		key += "@" + strings.Join(slices.Sorted(slices.Values(hosts)), ",")
	}
	if named {
		key += ":" + envVar
	}
	return key, grant, true
}

// grantHosts are the comma-joined hosts of a --grant value, trimmed and
// lowercased, without empties or repeats, in the order given.
func grantHosts(value string) []string {
	var hosts []string
	for _, host := range strings.Split(value, ",") {
		if host = strings.ToLower(strings.TrimSpace(host)); host != "" && !slices.Contains(hosts, host) {
			hosts = append(hosts, host)
		}
	}
	return hosts
}
