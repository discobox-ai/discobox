package pooloutbound

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
)

// NestedBootstrap uses only the enclosing sandbox's delivered runtime document.
// Ambient server HTTP_PROXY is deliberately irrelevant to recognizing nesting.
func NestedBootstrap(runtimeFile string) (*Intent, error) {
	raw, err := os.ReadFile(runtimeFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var delivered struct {
		SandboxID string            `json:"sandboxId"`
		ProxyEnvs []string          `json:"proxyEnvs"`
		Env       map[string]string `json:"env"`
	}
	if err := json.Unmarshal(raw, &delivered); err != nil {
		return nil, errors.New("invalid enclosing sandbox runtime document")
	}
	if delivered.SandboxID == "" || len(delivered.ProxyEnvs) == 0 {
		return nil, nil
	}
	allowed := map[string]bool{}
	for _, name := range delivered.ProxyEnvs {
		allowed[name] = true
	}
	lookup := func(name string) (string, bool) { v, ok := delivered.Env[name]; return v, ok && allowed[name] }
	resolved, err := Resolve(Intent{Mode: "inherit"}, lookup)
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
