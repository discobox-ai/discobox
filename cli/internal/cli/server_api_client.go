package cli

import (
	"net/http"
	"net/url"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
)

// Generated operations follow the current contract. Keep sending their legacy
// paths until the CLI and console can assume the server also serves the new
// paths. Translate before sending so even writes are issued only once.
func newServerAPIClient(baseURL string, client *http.Client) (*apiclientgen.Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	return apiclientgen.NewClient(baseURL, apiclientgen.WithClient(legacyServerAPIClient{
		basePath: strings.TrimRight(base.EscapedPath(), "/"),
		client:   client,
	}))
}

type legacyServerAPIClient struct {
	basePath string
	client   *http.Client
}

func (c legacyServerAPIClient) Do(r *http.Request) (*http.Response, error) {
	path, ok := strings.CutPrefix(r.URL.EscapedPath(), c.basePath)
	if !ok {
		return c.client.Do(r)
	}
	legacy := legacyServerAPIPath(path)
	if legacy == path {
		return c.client.Do(r)
	}
	rawPath := c.basePath + legacy
	decoded, err := url.PathUnescape(rawPath)
	if err != nil {
		return nil, err
	}
	req := r.WithContext(r.Context())
	u := *r.URL
	u.Path, u.RawPath = decoded, rawPath
	req.URL = &u
	return c.client.Do(req)
}

func legacyServerAPIPath(path string) string {
	if path == "/server/peer" {
		return "/peer"
	}
	parts := strings.SplitN(path, "/", 7)
	if len(parts) < 4 || parts[1] != "projects" {
		return path
	}
	if len(parts) == 4 && parts[3] == "events" {
		return "/api" + path
	}
	if len(parts) >= 6 && parts[3] == "sandboxes" {
		switch parts[5] {
		case "agent":
			if len(parts) == 7 {
				return "/api" + strings.Join(parts[:5], "/") + "/" + parts[6]
			}
		case "harness-config-changes":
			return "/api" + path
		}
	}
	return path
}
