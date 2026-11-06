package pooloutbound

import (
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
)

// enclosingRuntime is the delivered document, never ambient process state.
type enclosingRuntime struct {
	SandboxID string            `json:"sandboxId"`
	ProxyEnvs []string          `json:"proxyEnvs"`
	Env       map[string]string `json:"env"`
}

func readEnclosingRuntime(runtimeFile string) (*enclosingRuntime, error) {
	raw, err := os.ReadFile(runtimeFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var delivered enclosingRuntime
	if json.Unmarshal(raw, &delivered) != nil {
		return nil, errors.New("invalid enclosing sandbox runtime document")
	}
	if delivered.SandboxID == "" || len(delivered.ProxyEnvs) == 0 {
		return nil, nil
	}
	return &delivered, nil
}

func (d *enclosingRuntime) lookup(name string) (string, bool) {
	for _, allowed := range d.ProxyEnvs {
		if allowed == name {
			value, ok := d.Env[name]
			return value, ok
		}
	}
	return "", false
}

// recognizeInheritedForwarder retains recognized host reachability as a logical
// endpoint until the backend translates it. Only inherit calls this; explicit
// endpoints remain the operator's choice. Matching requires delivered material.
func recognizeInheritedForwarder(r Resolved, runtimeFile string) (Resolved, error) {
	candidate := false
	for _, raw := range []string{r.HTTP, r.HTTPS} {
		if raw == "" {
			continue
		}
		endpoint, _ := url.Parse(raw)
		ip := net.ParseIP(endpoint.Hostname())
		candidate = candidate || endpoint.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
	}
	if !candidate {
		return r, nil
	}
	delivered, err := readEnclosingRuntime(runtimeFile)
	if err != nil {
		return r, err
	}
	if delivered == nil {
		return r, nil
	}
	for _, endpoint := range []*string{&r.HTTP, &r.HTTPS} {
		if *endpoint == "" {
			continue
		}
		target, _ := url.Parse(*endpoint)
		ip := net.ParseIP(target.Hostname())
		if target.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			continue
		}
		for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
			raw, ok := delivered.lookup(name)
			if !ok || raw == "" {
				continue
			}
			known, err := url.Parse(raw)
			if err == nil && known.Scheme == target.Scheme && known.Host == target.Host {
				target.Host = LogicalForwarderHost
				*endpoint = target.String()
				break
			}
		}
	}
	return r, nil
}

// NestedBootstrap uses only the enclosing sandbox's delivered runtime document.
// Ambient server HTTP_PROXY is deliberately irrelevant to recognizing nesting.
func NestedBootstrap(runtimeFile string) (*Intent, error) {
	delivered, err := readEnclosingRuntime(runtimeFile)
	if err != nil || delivered == nil {
		return nil, err
	}
	resolved, err := Resolve(Intent{Mode: "inherit"}, delivered.lookup)
	if err != nil {
		return nil, err
	}
	if resolved.HTTP == "" && resolved.HTTPS == "" {
		return nil, nil
	}
	logical := "http://" + LogicalForwarderHost
	intent := &Intent{Mode: "explicit", CAPEM: resolved.CAPEM, NoProxy: strings.ReplaceAll(resolved.NoProxy, ",%LOCAL_SUBNETS%", "")}
	if resolved.HTTP != "" {
		intent.HTTP = logical
	}
	if resolved.HTTPS != "" {
		intent.HTTPS = logical
	}
	return intent, nil
}
