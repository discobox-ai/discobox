package buildkitagent

import (
	"log/slog"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/discobox-ai/discobox/proxy"
)

// The registry front: how a sandbox reaches the pool registry (ADR
// 26-10-29-160 §§1–2). The pool proxy forwards registry.discobox.internal here
// as the sandbox its client certificate names, in proxy.PoolClientHeader, and
// the front admits a request only for repositories under that sandbox's
// namespace — its ID — before the registry sees it.
//
// That header is the front's only word on who is asking, so it serves on a
// socket only the proxy reaches (layout.ProxyRegistrySocket), never a loopback
// port (pool-agent/REVIEW.md, "A listener the proxy forwards to").

// registryAPIRoot is the distribution API's base, which a client asks for
// first to learn the registry speaks it. It names no repository, so it is the
// one path outside a namespace the front answers.
const registryAPIRoot = "/v2/"

// RegistryFront forwards a sandbox's registry requests to the pool registry,
// within the sandbox's own namespace.
type RegistryFront struct {
	upstream *httputil.ReverseProxy
	logger   *slog.Logger
}

// NewRegistryFront returns a front that forwards to the registry at registry, a
// URL with no path, over transport: in the pool, https with the pool builder's
// certificate (RegistryClientTLS).
func NewRegistryFront(registry *url.URL, transport http.RoundTripper, logger *slog.Logger) *RegistryFront {
	if logger == nil {
		logger = slog.Default()
	}
	front := &RegistryFront{logger: logger}
	front.upstream = &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(registry)
			// The registry decides nothing by it; it stays the proxy's word,
			// and goes no further.
			r.Out.Header.Del(proxy.PoolClientHeader)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			front.logger.Warn("pool registry unreachable", "method", r.Method, "path", r.URL.Path, "error", err)
			http.Error(w, "the pool registry is unreachable", http.StatusBadGateway)
		},
	}
	return front
}

// ServeHTTP admits a request for the namespace of the sandbox the proxy names,
// and refuses everything else with a 403 the registry never sees.
func (f *RegistryFront) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	namespace := r.Header.Get(proxy.PoolClientHeader)
	if !validNamespace(namespace) {
		http.Error(w, "the proxy names no sandbox this registry has a namespace for", http.StatusForbidden)
		return
	}
	// The registry reads an upload's mount from a form body as well as from
	// the query (FormValue), and a form body carries nothing else a registry
	// request needs: refused, so the query below is the whole of it.
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType == "application/x-www-form-urlencoded" || mediaType == "multipart/form-data" {
		http.Error(w, "forbidden: a registry request carries no form", http.StatusForbidden)
		return
	}
	if reason := outsideNamespace(r.URL, namespace); reason != "" {
		http.Error(w, "forbidden: "+reason, http.StatusForbidden)
		return
	}
	r.URL.RawQuery = withinNamespaceMount(r.URL.Query(), namespace, r.URL.RawQuery)
	f.upstream.ServeHTTP(w, r)
}

// outsideNamespace says why a request's path is not the API root or a
// repository under namespace, or "" when it is one of those.
//
// The path is read as the registry's router will read it: one with escapes
// the plain encoding would not produce, or with an empty, `.` or `..` segment
// before its last, could be matched by the registry as a name the prefix here
// did not see, so it is refused rather than interpreted.
func outsideNamespace(u *url.URL, namespace string) string {
	if u.RawPath != "" {
		return "the path carries escapes no repository name needs"
	}
	if u.Path == registryAPIRoot || u.Path == strings.TrimSuffix(registryAPIRoot, "/") {
		return ""
	}
	rest, ok := strings.CutPrefix(u.Path, registryAPIRoot+namespace+"/")
	if !ok {
		return "only repositories under " + namespace + "/ are this sandbox's"
	}
	segments := strings.Split(rest, "/")
	for i, segment := range segments {
		last := i == len(segments)-1
		if segment == "." || segment == ".." || (segment == "" && !last) {
			return "the path is not a clean repository path"
		}
	}
	return ""
}

// withinNamespaceMount keeps a cross-repository blob mount only when the
// repository it mounts from is under namespace, and returns the query to
// forward.
//
// A mount from another sandbox's repository would link that sandbox's blob
// into this one's, which is a read of it. Dropping the mount, rather than
// refusing the request, is what a registry that may not mount does anyway:
// it opens an ordinary upload, and the client sends the blob itself.
func withinNamespaceMount(query url.Values, namespace, raw string) string {
	from, mounting := query["from"]
	if !mounting && !query.Has("mount") {
		return raw
	}
	if mounting && len(from) == 1 && (from[0] == namespace || strings.HasPrefix(from[0], namespace+"/")) {
		return raw
	}
	query.Del("mount")
	query.Del("from")
	return query.Encode()
}
