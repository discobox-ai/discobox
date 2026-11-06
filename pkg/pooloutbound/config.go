// Package pooloutbound owns resolved pool outbound routing and additional trust.
package pooloutbound

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const ManagedEnvironment = "DISCOBOX_PROXY_ENVIRONMENT=managed"

// Intent is a whole-object override. Empty endpoints in explicit mode mean direct.
// CAPEM contains public trust anchors, never a server-local filename.
type Intent struct {
	Mode    string `json:"mode"`
	HTTP    string `json:"http,omitempty"`
	HTTPS   string `json:"https,omitempty"`
	NoProxy string `json:"noProxy,omitempty"`
	CAPEM   string `json:"caPem,omitempty"`
}

// Resolved is private runtime material, resolved once by the server. A pointer to
// it distinguishes authoritative direct routing from standalone env discovery.
type Resolved struct {
	HTTP    string `json:"http,omitempty"`
	HTTPS   string `json:"https,omitempty"`
	NoProxy string `json:"noProxy,omitempty"`
	CAPEM   string `json:"caPem,omitempty"`
}

func ValidateEndpoint(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(raw, "\r\n\x00") {
		return errors.New("proxy endpoint must be an http:// URL with a host, optional Basic credentials, and no path, query or fragment")
	}
	if _, err := url.ParseRequestURI(raw); err != nil {
		return errors.New("invalid HTTP proxy endpoint")
	}
	return nil
}

func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid proxy>"
	}
	u.User = nil
	return u.String()
}

// Resolve never reads process environment unless mode explicitly selects inherit.
func Resolve(i Intent, lookup func(string) (string, bool)) (Resolved, error) {
	mode := i.Mode
	if mode == "" {
		mode = "direct"
	}
	if mode != "explicit" && (i.HTTP != "" || i.HTTPS != "" || i.NoProxy != "" || i.CAPEM != "") {
		return Resolved{}, errors.New("proxy payload requires mode explicit")
	}
	r := Resolved{}
	switch mode {
	case "direct":
	case "explicit":
		r = Resolved{HTTP: i.HTTP, HTTPS: i.HTTPS, NoProxy: i.NoProxy, CAPEM: i.CAPEM}
	case "inherit":
		first := func(names ...string) (string, bool) {
			for _, n := range names {
				if v, ok := lookup(n); ok {
					return v, true
				}
			}
			return "", false
		}
		all, _ := first("ALL_PROXY", "all_proxy")
		if ValidateEndpoint(all) != nil {
			all = ""
		}
		var ok bool
		r.HTTP, ok = first("HTTP_PROXY", "http_proxy")
		if !ok {
			r.HTTP = all
		}
		r.HTTPS, ok = first("HTTPS_PROXY", "https_proxy")
		if !ok {
			r.HTTPS = r.HTTP
		}
		r.NoProxy, _ = first("NO_PROXY", "no_proxy")
		file, _ := lookup("SSL_CERT_FILE")
		if file != "" {
			b, err := os.ReadFile(file)
			if err != nil {
				return r, fmt.Errorf("read inherited trust file: %w", err)
			}
			r.CAPEM = string(b)
		}
		dirs, _ := lookup("SSL_CERT_DIR")
		for _, dir := range filepath.SplitList(dirs) {
			if dir == "" {
				continue
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				return r, fmt.Errorf("read inherited trust directory: %w", err)
			}
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				b, err := os.ReadFile(filepath.Join(dir, e.Name()))
				if err != nil {
					return r, fmt.Errorf("read inherited trust anchor: %w", err)
				}
				r.CAPEM += "\n" + string(b)
			}
		}
	default:
		return r, errors.New("proxy mode must be direct, explicit or inherit")
	}
	if err := ValidateEndpoint(r.HTTP); err != nil {
		return r, fmt.Errorf("HTTP proxy: %w", err)
	}
	if err := ValidateEndpoint(r.HTTPS); err != nil {
		return r, fmt.Errorf("HTTPS proxy: %w", err)
	}
	if mode == "inherit" {
		var err error
		r, err = recognizeInheritedForwarder(r, "/etc/discobox/sandbox.json")
		if err != nil {
			return r, err
		}
	}
	if strings.ContainsAny(r.NoProxy, "\r\n\x00") {
		return r, errors.New("invalid proxy bypass list")
	}
	anchors, err := Anchors(r.CAPEM)
	if err != nil {
		return r, err
	}
	r.CAPEM = ""
	keys := make([]string, 0, len(anchors))
	for k := range anchors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r.CAPEM += anchors[k]
	}
	return r, nil
}

// Anchors canonicalizes a PEM certificate bundle by certificate fingerprint.
func Anchors(raw string) (map[string]string, error) {
	out := map[string]string{}
	rest := []byte(raw)
	for len(strings.TrimSpace(string(rest))) > 0 {
		block, tail := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, errors.New("additional trust must contain PEM certificates only")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA {
			return nil, errors.New("additional trust contains an invalid CA certificate")
		}
		sum := sha256.Sum256(cert.Raw)
		out[hex.EncodeToString(sum[:])] = string(pem.EncodeToMemory(block))
		rest = tail
	}
	return out, nil
}

func (r Resolved) Revision() string {
	b, _ := json.Marshal(r) //nolint:errchkjson // Resolved contains only strings; encoding cannot fail.
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Environment includes empty scheme values to suppress Docker's client proxy
// defaults. The marker is consumed by runcca before it injects enclosing trust.
func (r Resolved) Environment() []string {
	return []string{ManagedEnvironment, RevisionEnv + "=" + r.Revision(), "HTTP_PROXY=" + r.HTTP, "http_proxy=" + r.HTTP, "HTTPS_PROXY=" + r.HTTPS, "https_proxy=" + r.HTTPS, "NO_PROXY=" + r.NoProxy, "no_proxy=" + r.NoProxy, "ALL_PROXY=", "all_proxy="}
}

// Defaults holds already validated server configuration. Each level replaces
// the whole object; the API override is selected by the server before these.
type Defaults struct {
	GlobalMode string
	Modes      map[string]string
	Global     Resolved
	Configured bool
	ByPool     map[string]Resolved
}

func (d Defaults) Select(poolID string) (Resolved, string) {
	if r, ok := d.ByPool[poolID]; ok {
		return r, "yaml"
	}
	if d.Configured {
		return d.Global, "global"
	}
	return Resolved{}, "default"
}

// Delivery describes what a backend can apply without stopping its workloads.
type Delivery struct {
	// Host is configured by the engine over Docker copy/exec on an owned VM.
	Host            *Resolved `json:"-"`
	GuestProbes     *Probes   `json:"guestProbes,omitempty"`
	Runtime         Resolved  `json:"runtime"`
	AppliedRevision string    `json:"appliedRevision"`
	RestartRequired bool      `json:"restartRequired"`
}

// ResolveLocalForwarder resolves the bootstrap's logical endpoint at boot. It
// reads only the enclosing sandbox's delivered listener address, never its key.
func (r Resolved) ResolveLocalForwarder() (Resolved, error) {
	for _, endpoint := range []*string{&r.HTTP, &r.HTTPS} {
		if *endpoint == "" {
			continue
		}
		u, _ := url.Parse(*endpoint)
		if u.Hostname() != LogicalForwarderHost {
			continue
		}
		delivered, err := readEnclosingRuntime("/etc/discobox/sandbox.json")
		if err != nil || delivered == nil {
			return r, errors.New("enclosing sandbox forwarder material is unavailable or invalid")
		}
		var localEndpoint string
		for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
			if value, ok := delivered.lookup(name); ok && value != "" {
				localEndpoint = value
				break
			}
		}
		forwarder, err := url.Parse(localEndpoint)
		if err != nil || forwarder.Host == "" {
			return r, errors.New("enclosing sandbox forwarder is missing")
		}
		bridge := struct{ ListenAddress string }{forwarder.Host}
		u.Host = bridge.ListenAddress
		*endpoint = u.String()
	}
	return r, nil
}

// RejectLocalEndpoints is for hosted machines with no host/guest relay.
func (r Resolved) RejectLocalEndpoints() error {
	for _, raw := range []string{r.HTTP, r.HTTPS} {
		if raw == "" {
			continue
		}
		u, _ := url.Parse(raw)
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() == LogicalForwarderHost || strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && (ip.IsLoopback() || ip.IsUnspecified())) {
			return errors.New("host-local pool proxy requires a backend with a private relay; configure an upstream reachable from the pool machine")
		}
	}
	return nil
}

// WithInternalHosts adds only concrete control/runtime endpoints, never broad
// private ranges. Explicit operator bypass entries retain their usual meaning.
func (r Resolved) WithInternalHosts(hosts ...string) Resolved {
	entries := strings.Split(r.NoProxy, ",")
	entries = append(entries, "127.0.0.1", "localhost", "::1")
	for _, raw := range hosts {
		u, err := url.Parse(raw)
		if err == nil && u.Hostname() != "" {
			entries = append(entries, u.Hostname())
		}
	}
	seen := map[string]bool{}
	var result []string
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry != "" && !seen[entry] {
			seen[entry] = true
			result = append(result, entry)
		}
	}
	r.NoProxy = strings.Join(result, ",")
	return r
}

// ValidateEnvironment checks the actual provisioning exec boundary, including
// wrappers that predate the managed marker. It never reports variable values.
func (r Resolved) ValidateEnvironment(lookup func(string) (string, bool)) error {
	for _, entry := range r.Environment()[1:] {
		name, want, _ := strings.Cut(entry, "=")
		got, _ := lookup(name)
		if got != want {
			return fmt.Errorf("managed pool proxy environment was overridden at %s; the hosting daemon must preserve create and exec environment", name)
		}
	}
	for _, name := range []string{"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "NODE_USE_ENV_PROXY", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "PIP_CERT", "GIT_SSL_CAINFO"} {
		if got, _ := lookup(name); got != "" {
			return fmt.Errorf("hosting runtime injected %s into a managed pool exec; upgrade the enclosing sandbox runtime to support managed proxy environments", name)
		}
	}
	return nil
}

// RedactText removes URL credentials and their Basic encoding from diagnostics.
func (r Resolved) RedactText(message string) string {
	for _, raw := range []string{r.HTTP, r.HTTPS} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.User == nil {
			continue
		}
		message = strings.ReplaceAll(message, raw, Redact(raw))
		message = strings.ReplaceAll(message, u.User.String()+"@", "[redacted]@")
		password, _ := u.User.Password()
		basic := u.User.Username() + ":" + password
		message = strings.ReplaceAll(message, basic+"@", "[redacted]@")
		message = strings.ReplaceAll(message, base64.StdEncoding.EncodeToString([]byte(basic)), "[redacted]")
	}
	return message
}

type privateError struct {
	cause   error
	message string
}

func (e *privateError) Error() string { return e.message }
func (e *privateError) Unwrap() error { return e.cause }

// RedactError keeps error identity while removing private proxy material.
func (r Resolved) RedactError(err error) error {
	if err == nil {
		return nil
	}
	message := r.RedactText(err.Error())
	if message == err.Error() {
		return err
	}
	return &privateError{cause: err, message: message}
}
