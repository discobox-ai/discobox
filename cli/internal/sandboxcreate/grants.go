package sandboxcreate

import (
	"fmt"
	"slices"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// ParseGrants reads --grant values into the grants a create gives the new
// discobox. A value is SECRET[@HOST]:ENV_VAR: the secret, the variable the new
// discobox receives its sentinel in, and the hosts it may be sent to. HOST may
// be several, joined by commas, for a credential sent to more than one site
// (ADR 26-10-02-393); none leaves the list out for the secret's own host. A
// secret is left as written, for the caller to resolve to an ID. One variable
// is filled once, so two values naming it are refused.
func ParseGrants(values []string) ([]apimodel.SandboxGrant, error) {
	var grants []apimodel.SandboxGrant
	seen := map[string]bool{}
	for _, value := range values {
		grant, envVar, valid := grantSpec(value)
		if !valid {
			return nil, fmt.Errorf("--grant %q: want SECRET[@HOST[,HOST...]]:ENV_VAR, such as github@github.com:GH_TOKEN", value)
		}
		if seen[envVar] {
			return nil, fmt.Errorf("--grant %q: %s is already given; a variable holds one credential", value, envVar)
		}
		seen[envVar] = true
		grants = append(grants, grant)
	}
	return grants, nil
}

// grantSpec reads a --grant value: the grant it is, and the variable it fills.
// It is not valid without a secret or a variable.
func grantSpec(value string) (apimodel.SandboxGrant, string, bool) {
	target, envVar, named := strings.Cut(value, ":")
	credential, host, _ := strings.Cut(target, "@")
	credential, envVar = strings.TrimSpace(credential), strings.TrimSpace(envVar)
	var grant apimodel.SandboxGrant
	if !named || credential == "" || envVar == "" || strings.ContainsAny(envVar, "=:@ ") {
		return grant, "", false
	}
	grant.SetSecretId(apiclientgen.NewOptString(credential))
	grant.SetEnvVar(apiclientgen.NewOptString(envVar))
	// None named leaves the list out, which takes the secret's host.
	grant.SetHosts(grantHosts(host))
	return grant, envVar, true
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
