// Package wellknown is the registry of well-known credentials: credentials
// Discobox knows the shape of, named by a reverse-DNS ID, so an agent can ask
// for one by ID and a request carries the right host and variable without the
// agent spelling them.
//
// It is shared by everything that meets an ID — the pool's credential broker,
// which expands one into a request, and the control plane, which checks it and
// binds the secret that answers it — so they cannot disagree about what an ID
// means.
package wellknown

import (
	"slices"

	"github.com/discobox-ai/discobox/hostscope"
)

// GitHubAPI is GitHub: the site git pushes and pulls over HTTPS, and the REST
// and GraphQL API beneath it.
const GitHubAPI = "com.github.api"

// Credential is one entry in the registry. A request for it is fulfilled by the
// project secret marked with its ID, which a person chooses the first time
// they approve a request for it.
type Credential struct {
	ID string
	// Name is what the credential is called when a request names it.
	Name string
	// Description says what the credential is for, in a sentence an agent and
	// an approver both read.
	Description string
	// Hosts are where the credential is sent. A request is for the first, and
	// each covers the hosts beneath it (hostscope.Covers), so one entry
	// stands for a site and its subdomains.
	Hosts []string
	// EnvVar is the variable it is delivered in.
	EnvVar string
}

// Host is the host a request for the credential names.
func (c Credential) Host() string { return c.Hosts[0] }

// AllowsHost reports whether host is one the credential is sent to: one of its
// hosts, or a host beneath one. An ask may name the narrower host it will
// actually reach — api.github.com under github.com — and be granted that alone.
func (c Credential) AllowsHost(host string) bool {
	return slices.ContainsFunc(c.Hosts, func(scope string) bool { return hostscope.Covers(scope, host) })
}

var registry = []Credential{
	{
		ID:          GitHubAPI,
		Name:        "github",
		Description: "GitHub: repositories over HTTPS as git pushes and pulls them, and the REST and GraphQL API beneath the same site, as gh uses it.",
		Hosts:       []string{"github.com"},
		EnvVar:      "GH_TOKEN",
	},
}

// Lookup returns the credential with the given ID.
func Lookup(id string) (Credential, bool) {
	for _, c := range registry {
		if c.ID == id {
			return c, true
		}
	}
	return Credential{}, false
}

// All returns every well-known credential, in registry order.
func All() []Credential { return slices.Clone(registry) }
