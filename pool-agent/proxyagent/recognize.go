package proxyagent

import (
	"encoding/json"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/proxy"
)

// Recognizing a request before it is judged (ADR 26-09-26-240 §1).
//
// The proxy knows a body only as a media type and a length, and the judge only
// as bytes; nothing between them knows what a protocol or an API means by it.
// Recognition is that knowledge, on two independent axes: the protocol a
// request speaks, whatever host it goes to, and the endpoint it calls on one
// API. Each is a built-in, ordered registry — structured to be added to, not
// configured. Nothing a request, a sandbox, or a use says adds to it.
//
// Recognition only adds information. The sandbox writes every byte, so a
// request can claim to be anything; one that claims a protocol and does not
// parse as it is shown as having failed to (judge.Body.ParseError), which the
// judge weighs, and never quietly described as something weaker.

// protocol is a wire protocol a request may speak.
type protocol struct {
	judge.Recognition
	// matches reports whether a request claims to be this protocol, from
	// what identifies it: the method, the path, the declared media type.
	matches func(req proxy.SecretAuthorizeRequest) bool
	// parser reads the protocol's bodies. It outranks the one the media
	// type would choose, since the protocol says what the bytes mean.
	parser *bodyParser
	// refuse answers a refused request the way the protocol's own server
	// would, so its client shows why (ADR 26-09-26-240 §5). Nil is the
	// proxy's plain refusal.
	refuse func(body []byte, complete bool, said string) (proxy.SecretRefusal, bool)
}

// protocols is every protocol a pool recognizes, in the order they are tried.
var protocols = []*protocol{gitReceivePack}

// endpoint is one operation on one API.
type endpoint struct {
	judge.Recognition
	// host is the API's host, as the request names it without a port.
	host string
	// route is the method and path, in the pattern syntax a standing allow
	// uses.
	route judge.Route
	// parser is what the API reads the body as, whatever the request labels
	// it: GitHub reads a fork's body as JSON under any content type, so a
	// body labeled otherwise must still be read as the JSON it is to the
	// API, or the one field that says where the fork lands goes unseen.
	parser *bodyParser
	// describe lifts what says what the operation does out of a JSON body
	// that is one whole object, into the metadata from the first ask beside
	// the parser's own: where a fork lands, what a new discobox is granted.
	// Each field is worth a round on its own, and only the endpoint knows
	// which ones they are. Nil lifts nothing.
	describe func(fields map[string]json.RawMessage, in parsedBody) map[string]any
}

// endpoints is every endpoint a pool recognizes, in the order they are tried.
var endpoints = []*endpoint{
	mustEndpoint(judge.EndpointGitHubFork, 1, "api.github.com", "POST /repos/{owner}/{repo}/forks", jsonParser,
		liftValues("organization", "name", "default_branch_only")),
	mustEndpoint(judge.EndpointDiscoboxSandboxCreate, 4, GateHost(), "POST /projects/{project}/sandboxes", jsonParser,
		describeSandboxCreate),
	// What `discobox new` sends after a create to finish making the
	// discobox: it polls it, pushes its source into its origin, and reports
	// the push done. Each is judged on its own, so each is named.
	mustEndpoint(judge.EndpointDiscoboxSandboxGet, 1, GateHost(), "GET /projects/{project}/sandboxes/{sandbox}", nil, nil),
	mustEndpoint(judge.EndpointDiscoboxSandboxOriginRefs, 2, GateHost(),
		"GET /projects/{project}/sandboxes/{sandbox}/git-origins/{repo}/info/refs", nil, nil),
	mustEndpoint(judge.EndpointDiscoboxSandboxOriginPush, 2, GateHost(),
		"POST /projects/{project}/sandboxes/{sandbox}/git-origins/{repo}/git-receive-pack", nil, nil),
	mustEndpoint(judge.EndpointDiscoboxSandboxSourcePushed, 2, GateHost(),
		"POST /projects/{project}/sandboxes/{sandbox}/complete-source-push", jsonParser, describeSourcePushed),
}

func mustEndpoint(name string, version int, host, pattern string, parser *bodyParser,
	describe func(map[string]json.RawMessage, parsedBody) map[string]any,
) *endpoint {
	route, err := judge.ParseRoute(pattern)
	if err != nil {
		panic("proxyagent: endpoint " + name + ": " + err.Error())
	}
	return &endpoint{
		Recognition: judge.Recognition{Name: name, Version: version},
		host:        host, route: route, parser: parser, describe: describe,
	}
}

// recognition is what one request was recognized as. Either may be nil.
type recognition struct {
	protocol *protocol
	endpoint *endpoint
}

// recognize reads what a request is. It reads only what identifies the
// operation — never the body — so it costs nothing to ask again, and every
// round of one request recognizes it the same way.
func recognize(req proxy.SecretAuthorizeRequest) recognition {
	var out recognition
	for _, candidate := range protocols {
		if candidate.matches(req) {
			out.protocol = candidate
			break
		}
	}
	host := strings.ToLower(req.Host)
	for _, candidate := range endpoints {
		if candidate.host == host && candidate.route.Matches(req.Method, req.URL) {
			out.endpoint = candidate
			break
		}
	}
	return out
}

// named is what a request was recognized as, as the judge is told it.
func (r recognition) named() (protocolName, endpointName *judge.Recognition) {
	if r.protocol != nil {
		recognized := r.protocol.Recognition
		protocolName = &recognized
	}
	if r.endpoint != nil {
		recognized := r.endpoint.Recognition
		endpointName = &recognized
	}
	return protocolName, endpointName
}

// parser is what reads this request's body: the protocol's, when it has one,
// then the endpoint's, and otherwise the one its media type calls for. What
// the receiving end will read the bytes as outranks what the sandbox labeled
// them.
func (r recognition) parser(req proxy.SecretAuthorizeRequest) *bodyParser {
	switch {
	case r.protocol != nil && r.protocol.parser != nil:
		return r.protocol.parser
	case r.endpoint != nil && r.endpoint.parser != nil:
		return r.endpoint.parser
	default:
		return parserForMediaType(req.Header.Get("Content-Type"))
	}
}

// requestPath is the unescaped path a request goes to, or nothing when its
// URL cannot be read.
func requestPath(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Path
}

// mediaTypeOf is a declared content type without its parameters, lower case.
func mediaTypeOf(header http.Header) string {
	media, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		return strings.ToLower(strings.TrimSpace(header.Get("Content-Type")))
	}
	return media
}
