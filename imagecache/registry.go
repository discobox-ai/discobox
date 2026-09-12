package imagecache

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// acceptManifests is every manifest shape staging understands, index and image,
// OCI and Docker. Offering them all is what gets a tag answered with its index
// rather than with a platform the registry picked on the client's behalf.
var acceptManifests = strings.Join([]string{
	mediaTypeOCIIndex, mediaTypeDockerList, mediaTypeOCIManifest, mediaTypeDockerManifest,
}, ", ")

// stallTimeout ends a blob download that has stopped arriving. Generous,
// because a slow link is not a stalled one: what is measured is silence, not
// duration — the same rule serverstage applies to the server's own download.
//
// A var rather than a const only so a test can shorten it.
var stallTimeout = 60 * time.Second

// Credentials authorize fetches from a registry that wants them.
type Credentials struct {
	// Username and Password log in to the registry's token service, or to the
	// registry itself when it asks for Basic authorization.
	Username string
	Password string
	// Token is a registry token, presented as it is rather than exchanged.
	Token string
}

// registry is the part of the OCI distribution API staging uses: a manifest
// GET, a blob GET, and the authorization a registry asks for before either.
//
// Anonymous unless the caller has credentials to offer. The CLI never does:
// what it stages is a release's own images, which are public, and a CLI that
// went looking for registry credentials would be reading a keychain on the
// strength of a manifest it was linked with. The server does, from its own
// registry keychain, because a provider fetches through the same store and a
// guest image it was configured with may be private (ADR 0113 §5).
type registry struct {
	client      *http.Client
	credentials func(ctx context.Context, registry string) Credentials

	mu sync.Mutex
	// authorizations is the Authorization header that last worked, per
	// repository.
	authorizations map[string]string
	// plain records the registries on this machine that answered only over
	// plain HTTP.
	plain map[string]bool
}

func newRegistry(client *http.Client, credentials func(context.Context, string) Credentials) *registry {
	if client == nil {
		client = defaultClient()
	}
	return &registry{client: client, credentials: credentials, authorizations: map[string]string{}, plain: map[string]bool{}}
}

// scheme is how a registry is spoken to: HTTPS, unless it is on this machine
// and has already turned out to speak only plain HTTP.
//
// That is Docker's rule for localhost and the loopback range, and
// go-containerregistry's, so a development registry a guest image was pushed
// to works here as it did there — and no registry anywhere else is ever read
// without TLS. Integrity is the digests' either way.
func (r *registry) scheme(host string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.plain[host] {
		return "http"
	}
	return "https"
}

func (r *registry) speaksPlain(host string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plain[host] = true
}

// onThisMachine reports whether a registry host is localhost or a loopback
// address, the only registries plain HTTP is tried for.
func onThisMachine(host string) bool {
	hostname := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostname = h
	}
	if hostname == "localhost" {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

// get requests a path under the reference's repository, authorizing once if
// the registry asks — which it also does when a token has expired partway
// through a long staging, so an expiry costs one round trip.
func (r *registry) get(ctx context.Context, ref Reference, path, accept string) (*http.Response, error) {
	host := ref.apiHost()
	// A token is scoped to one repository, so authorization is kept per
	// repository: five images from one registry are five scopes.
	key := host + "/" + ref.Repository
	send := func(authorization string) (*http.Response, error) {
		target := r.scheme(host) + "://" + host + "/v2/" + ref.Repository + path
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if authorization != "" {
			// Go drops this header on a redirect to another host, which is what
			// a registry does with a blob — ghcr.io hands it to a CDN — and is
			// exactly right: the authorization is the registry's, not the CDN's.
			req.Header.Set("Authorization", authorization)
		}
		return r.client.Do(req)
	}
	resp, err := send(r.authorization(key))
	if err != nil && ctx.Err() == nil && r.scheme(host) == "https" && onThisMachine(host) {
		r.speaksPlain(host)
		resp, err = send(r.authorization(key))
	}
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	drainAndClose(resp)
	authorization, err := r.authorize(ctx, challenge, ref)
	if err != nil {
		return nil, fmt.Errorf("authorize %s: %w", ref.qualifiedRepository(), err)
	}
	r.setAuthorization(key, authorization)
	return send(authorization)
}

func (r *registry) authorization(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.authorizations[key]
}

func (r *registry) setAuthorization(key, authorization string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.authorizations[key] = authorization
}

// authorize answers a registry's challenge: with a bearer token from its token
// service — anonymous, or logged in with whatever credentials the caller has
// for the registry — or with those credentials directly when it asks for Basic.
func (r *registry) authorize(ctx context.Context, challenge string, ref Reference) (string, error) {
	scheme, params := parseChallenge(challenge)
	var credentials Credentials
	if r.credentials != nil {
		credentials = r.credentials(ctx, ref.Domain)
	}
	switch {
	case strings.EqualFold(scheme, "Bearer"):
		if credentials.Token != "" {
			return "Bearer " + credentials.Token, nil
		}
		token, err := r.fetchToken(ctx, params, ref, credentials)
		if err != nil {
			return "", err
		}
		return "Bearer " + token, nil
	case strings.EqualFold(scheme, "Basic"):
		if credentials.Username == "" {
			return "", errors.New("the registry wants a login, and there are no credentials for it")
		}
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(credentials.Username+":"+credentials.Password)), nil
	case scheme == "":
		return "", errors.New("the registry refused the request and said nothing about how to authorize one")
	default:
		return "", fmt.Errorf("the registry wants %s authorization, which is not spoken here", scheme)
	}
}

// fetchToken asks a Bearer challenge's token service for a token, logged in
// when there are credentials to log in with.
func (r *registry) fetchToken(ctx context.Context, params map[string]string, ref Reference, credentials Credentials) (string, error) {
	realm, err := url.Parse(params["realm"])
	if err != nil || (realm.Scheme != "https" && realm.Scheme != "http") || realm.Host == "" {
		return "", fmt.Errorf("the registry named an unusable token realm %q", params["realm"])
	}
	query := realm.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + ref.Repository + ":pull"
	}
	query.Set("scope", scope)
	realm.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", err
	}
	if credentials.Username != "" {
		req.SetBasicAuth(credentials.Username, credentials.Password)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", err
	}
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request: %s", resp.Status)
	}
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	token := body.Token
	if token == "" {
		token = body.AccessToken
	}
	if token == "" {
		return "", errors.New("token request: the registry returned no token")
	}
	return token, nil
}

// parseChallenge splits a WWW-Authenticate value into its scheme and
// parameters. A quoted value may hold commas — a scope naming two actions does
// — so this reads the grammar rather than splitting on them.
func parseChallenge(header string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(header), " ")
	params := map[string]string{}
	rest = strings.TrimSpace(rest)
	for rest != "" {
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(rest[:eq]))
		rest = strings.TrimSpace(rest[eq+1:])
		var value strings.Builder
		if strings.HasPrefix(rest, `"`) {
			rest = rest[1:]
			for rest != "" {
				c := rest[0]
				rest = rest[1:]
				if c == '\\' && rest != "" {
					value.WriteByte(rest[0])
					rest = rest[1:]
					continue
				}
				if c == '"' {
					break
				}
				value.WriteByte(c)
			}
		} else {
			end := strings.IndexByte(rest, ',')
			if end < 0 {
				end = len(rest)
			}
			value.WriteString(strings.TrimSpace(rest[:end]))
			rest = rest[end:]
		}
		params[key] = value.String()
		rest = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ","))
	}
	return scheme, params
}

// manifest fetches a manifest by tag or digest and describes it by the digest
// of the bytes received.
//
// Asked for by digest, the bytes must hash to it. Asked for by tag, the
// registry's own Docker-Content-Digest must agree with them when it states one:
// that is the digest a daemon pulling the same tag would record, and so the one
// a sandbox gets pinned to.
func (r *registry) manifest(ctx context.Context, ref Reference, pinned string) ([]byte, Descriptor, error) {
	resp, err := r.get(ctx, ref, "/manifests/"+pinned, acceptManifests)
	if err != nil {
		return nil, Descriptor{}, fmt.Errorf("fetch %s: %w", ref.Name(), err)
	}
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, Descriptor{}, fmt.Errorf("fetch %s: %s", ref.Name(), resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, Descriptor{}, fmt.Errorf("fetch %s: %w", ref.Name(), err)
	}
	if len(data) > maxManifestBytes {
		return nil, Descriptor{}, fmt.Errorf("fetch %s: the manifest is larger than %d bytes", ref.Name(), maxManifestBytes)
	}
	digest := digestOf(data)
	if strings.HasPrefix(pinned, "sha256:") && digest != pinned {
		return nil, Descriptor{}, fmt.Errorf("fetch %s: the registry served %s for %s", ref.Name(), digest, pinned)
	}
	if stated := resp.Header.Get("Docker-Content-Digest"); stated != "" && stated != digest {
		return nil, Descriptor{}, fmt.Errorf("fetch %s: the registry says it is %s, and its bytes are %s", ref.Name(), stated, digest)
	}
	mediaType := manifestMediaType(data, resp.Header.Get("Content-Type"))
	if !isIndex(mediaType) && !isManifest(mediaType) {
		return nil, Descriptor{}, fmt.Errorf("fetch %s: unsupported manifest type %q", ref.Name(), mediaType)
	}
	return data, Descriptor{MediaType: mediaType, Digest: digest, Size: int64(len(data))}, nil
}

// manifestMediaType is what a manifest says it is, or what the response said
// when the manifest does not: the field is optional in an OCI manifest.
func manifestMediaType(data []byte, contentType string) string {
	var probe struct {
		MediaType string `json:"mediaType"`
	}
	if json.Unmarshal(data, &probe) == nil && probe.MediaType != "" {
		return probe.MediaType
	}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	return mediaType
}

// blob streams d's bytes into w. At most one byte past the declared size is
// read, which is enough for the caller to tell "exactly right" from "more than
// promised" without letting a source serving something else fill the disk.
func (r *registry) blob(ctx context.Context, ref Reference, d Descriptor, w io.Writer) error {
	// Canceled by the stall watch as well as by the caller: a read blocked
	// inside a body that stopped arriving is only ended by its context.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := r.get(ctx, ref, "/blobs/"+d.Digest, "")
	if err != nil {
		return err
	}
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status)
	}
	watch := newStallWatch(cancel)
	defer watch.stop()
	_, err = io.Copy(w, &stallReader{reader: io.LimitReader(resp.Body, d.Size+1), watch: watch})
	if err != nil && watch.stalled.Load() {
		return fmt.Errorf("nothing arrived for %s", stallTimeout)
	}
	return err
}

// stallWatch cancels a download once nothing has arrived for stallTimeout.
type stallWatch struct {
	timer   *time.Timer
	stalled atomic.Bool
}

func newStallWatch(cancel context.CancelFunc) *stallWatch {
	w := &stallWatch{}
	w.timer = time.AfterFunc(stallTimeout, func() {
		w.stalled.Store(true)
		cancel()
	})
	return w
}

func (w *stallWatch) stop() {
	w.timer.Stop()
}

type stallReader struct {
	reader io.Reader
	watch  *stallWatch
}

func (r *stallReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.watch.timer.Reset(stallTimeout)
	}
	return n, err
}

func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// defaultClient is what fetches from a registry when a caller supplies none.
//
// No Client.Timeout: that bounds the whole exchange including the body, and a
// gigabyte layer over a slow link is a long exchange that is working fine.
// Each step where silence means failure is bounded instead, and the body by
// the stall watch.
func defaultClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: time.Second,
			ForceAttemptHTTP2:     true,
		},
	}
}
