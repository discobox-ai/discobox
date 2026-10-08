package proxyagent

import (
	"net/url"
)

// The origins host: where a sandbox fetches its sources' origins, which the
// pool proxy answers by forwarding to this pool agent rather than sending to
// the internet (ADR 0126 §4, ADR 26-10-08-561).

const (
	// OriginsHost is the host a sandbox's origin remotes name. It sits beside
	// the gate host (GateHost, api.discobox.internal) under the same reserved
	// name: both are the pool's, answered by its proxy, and neither resolves
	// anywhere else.
	OriginsHost = "git.discobox.internal"

	// OriginsListenAddress is where the pool agent serves its sandboxes'
	// origins to the proxy. Loopback, like ControlListenAddress: the proxy
	// unit shares the pool's network namespace with the agent, and nothing a
	// sandbox runs can reach it except through the proxy, which says which
	// sandbox is asking.
	OriginsListenAddress = "127.0.0.1:17086"
)

// OriginURL is a source's origin as its sandbox fetches it: the pool's
// git-origins route, at the origins host.
func OriginURL(projectID, poolID, sandboxID, slug string) string {
	return (&url.URL{
		Scheme: "https",
		Host:   OriginsHost,
		Path:   "/api/project/" + projectID + "/pool/" + poolID + "/sandboxes/" + sandboxID + "/git-origins/" + slug + ".git",
	}).String()
}
