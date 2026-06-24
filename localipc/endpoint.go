package localipc

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const LogicalHTTPBaseURL = "http://discobox.local"

type Endpoint struct {
	Raw    string
	Scheme string
	Value  string
}

func Parse(raw string) (Endpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Endpoint{}, fmt.Errorf("endpoint is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Endpoint{}, err
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https":
		if u.Host == "" {
			return Endpoint{}, fmt.Errorf("%s endpoint %q must include a host", scheme, raw)
		}
		return Endpoint{Raw: raw, Scheme: scheme, Value: strings.TrimRight(raw, "/")}, nil
	case "unix":
		if u.Path == "" {
			return Endpoint{}, fmt.Errorf("unix endpoint %q must include a socket path", raw)
		}
		return Endpoint{Raw: raw, Scheme: scheme, Value: u.Path}, nil
	case "npipe":
		value := npipePath(u)
		if value == "" {
			return Endpoint{}, fmt.Errorf("npipe endpoint %q must include a pipe path", raw)
		}
		return Endpoint{Raw: raw, Scheme: scheme, Value: value}, nil
	default:
		return Endpoint{}, fmt.Errorf("unsupported endpoint scheme %q in %q", u.Scheme, raw)
	}
}

func npipePath(u *url.URL) string {
	value := u.Host + u.Path
	if value == "" {
		value = u.Opaque
	}
	value = strings.ReplaceAll(value, "/", `\`)
	if strings.HasPrefix(value, `\\.\pipe\`) {
		return value
	}
	value = strings.TrimPrefix(value, `\`)
	if value == "" {
		return ""
	}
	return `\\.\pipe\` + value
}

func HTTPClient(endpoint string, base http.RoundTripper) (baseURL string, client *http.Client, err error) {
	parsed, err := Parse(endpoint)
	if err != nil {
		return "", nil, err
	}
	switch parsed.Scheme {
	case "http", "https":
		if base == nil {
			return parsed.Value, http.DefaultClient, nil
		}
		return parsed.Value, &http.Client{Transport: base}, nil
	case "unix":
		transport, err := unixRoundTripper(parsed.Value, base)
		if err != nil {
			return "", nil, err
		}
		return LogicalHTTPBaseURL, &http.Client{Transport: transport}, nil
	case "npipe":
		transport, err := npipeRoundTripper(parsed.Value, base)
		if err != nil {
			return "", nil, err
		}
		return LogicalHTTPBaseURL, &http.Client{Transport: transport}, nil
	default:
		return "", nil, fmt.Errorf("unsupported endpoint scheme %q", parsed.Scheme)
	}
}
