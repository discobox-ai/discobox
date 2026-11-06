package pooloutbound

import (
	"os"
	"strings"
	"testing"
)

func TestResolutionIsExplicitAndPreservesEmptyInheritance(t *testing.T) {
	cases := []struct {
		name        string
		intent      Intent
		env         map[string]string
		http, https string
		bad         bool
	}{
		{name: "empty inherited trust directories", intent: Intent{Mode: "inherit"}, env: map[string]string{"SSL_CERT_DIR": string(os.PathListSeparator)}},
		{name: "default ignores ambient", env: map[string]string{"HTTP_PROXY": "http://ambient:1"}},
		{name: "explicit localhost remains operator choice", intent: Intent{Mode: "explicit", HTTP: "http://127.0.0.1:17008"}, http: "http://127.0.0.1:17008"},
		{name: "explicit empty scheme", intent: Intent{Mode: "explicit", HTTP: "http://first:1"}, env: map[string]string{"HTTPS_PROXY": "http://ambient:1"}, http: "http://first:1"},
		{name: "independent inherited schemes", intent: Intent{Mode: "inherit"}, env: map[string]string{"HTTP_PROXY": "http://first:1", "HTTPS_PROXY": "http://second:2"}, http: "http://first:1", https: "http://second:2"},
		{name: "empty uppercase suppresses lowercase and fallback", intent: Intent{Mode: "inherit"}, env: map[string]string{"HTTP_PROXY": "", "http_proxy": "http://lower:1", "HTTPS_PROXY": "", "ALL_PROXY": "http://all:1"}},
		{name: "missing https uses http", intent: Intent{Mode: "inherit"}, env: map[string]string{"HTTP_PROXY": "http://first:1"}, http: "http://first:1", https: "http://first:1"},
		{name: "socks fallback ignored", intent: Intent{Mode: "inherit"}, env: map[string]string{"ALL_PROXY": "socks5://ignored:1"}},
		{name: "ambiguous payload", intent: Intent{HTTP: "http://first:1"}, bad: true},
		//nolint:gosec // Deliberately invalid fixture checks credential redaction.
		{name: "credentials never in error", intent: Intent{Mode: "explicit", HTTP: "http://private:" + "secret@bad host:1"}, bad: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Resolve(c.intent, func(k string) (string, bool) { v, ok := c.env[k]; return v, ok })
			if (err != nil) != c.bad {
				t.Fatalf("error=%v", err)
			}
			if err != nil && (strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private")) {
				t.Fatal("credentials leaked")
			}
			if !c.bad && (got.HTTP != c.http || got.HTTPS != c.https) {
				t.Fatalf("routing = %+v", got)
			}
		})
	}
}

func TestUnsupportedProxyURLs(t *testing.T) {
	for _, u := range []string{"https://proxy:3128", "socks5://proxy:1", "http://proxy/path", "http://proxy?x=1", "http://proxy#fragment", "http://proxy?"} {
		if ValidateEndpoint(u) == nil {
			t.Errorf("accepted %s", u)
		}
	}
	if err := ValidateEndpoint("http://user:p%40ss@proxy:3128/"); err != nil {
		t.Fatal(err)
	}
}

func TestWholeObjectSelection(t *testing.T) {
	defaults := Defaults{Global: Resolved{HTTP: "http://global:1", HTTPS: "http://global:2"}, Configured: true, ByPool: map[string]Resolved{"pool_a": {HTTP: "http://pool-a:1"}, "pool_b": {}}}
	r, source := defaults.Select("pool_a")
	if source != "yaml" || r.HTTPS != "" {
		t.Fatalf("merged objects: %+v", r)
	}
	r, _ = defaults.Select("pool_b")
	if r.HTTP != "" || r.HTTPS != "" {
		t.Fatalf("direct override: %+v", r)
	}
	r, _ = defaults.Select("pool_c")
	if r.HTTPS != "http://global:2" {
		t.Fatalf("global: %+v", r)
	}
}

func TestEmptySchemeIsAlsoDirectForAPT(t *testing.T) {
	r := Resolved{HTTP: "http://proxy:3128"}
	config := r.APTConfig()
	if !strings.Contains(config, `Acquire::http::Proxy "http://proxy:3128";`) || !strings.Contains(config, `Acquire::https::Proxy "DIRECT";`) {
		t.Fatal(config)
	}
}
