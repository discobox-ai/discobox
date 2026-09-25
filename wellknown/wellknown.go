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
	"time"

	"github.com/discobox-ai/discobox/hostscope"
)

// GitHubAPI is GitHub: the site git pushes and pulls over HTTPS, and the REST
// and GraphQL API beneath it.
const GitHubAPI = "com.github.api"

// DiscoboxSandbox is the discobox API, as a discobox calls it through its pool
// (ADR 0140).
const DiscoboxSandbox = "ai.discobox.sandbox"

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
	// Gate is a credential with nothing behind its sentinel: the pool admits
	// a request carrying a live use of it to its host, and never swaps a
	// value in. Approving a request for one chooses no secret.
	Gate bool
	// RefreshCommand is the command that prints the credential on a machine
	// that is logged in, as an argument vector. It is what a person is offered
	// when they give a secret for the ID, and they may edit it before it is
	// saved; nothing runs it from here (ADR 26-09-25-122 §2). A gate has none.
	RefreshCommand []string
	// RefreshTTL is how long a value from RefreshCommand is trusted before a
	// new one is asked for, when the person storing it chooses nothing else.
	// It follows how long the credential really lives: a value that outlasts
	// it is only re-checked, since a refusal asks for a new one at once. Zero
	// takes the default for any command (ADR 26-09-25-122 §1).
	RefreshTTL time.Duration
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
		ID:             GitHubAPI,
		Name:           "github",
		Description:    "GitHub: repositories over HTTPS as git pushes and pulls them, and the REST and GraphQL API beneath the same site, as gh uses it.",
		Hosts:          []string{"github.com"},
		EnvVar:         "GH_TOKEN",
		RefreshCommand: []string{"gh", "auth", "token"},
		// gh's token is an OAuth app token: it lasts until it is revoked or
		// gh logs in again, so a day is a re-check, not an expiry.
		RefreshTTL: 24 * time.Hour,
	},
	{
		ID:          DiscoboxSandbox,
		Name:        "discobox",
		Description: "The discobox API, reached through this discobox's pool: create, list, and get discoboxes, give a new one uses of project secrets, and answer credential requests.",
		Hosts:       []string{"api.discobox.internal"},
		EnvVar:      "DISCOBOX_TOKEN",
		Gate:        true,
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
