package model

import (
	"encoding/json"
	"testing"

	"github.com/discobox-ai/discobox/pkg/secretroute"
)

func TestHarnessConstraintsNeverReopenAnOldDenial(t *testing.T) {
	sec := &Secret{Routes: []SecretRoute{
		{Hosts: []string{"*"}},
		{Hosts: []string{"api.anthropic.com"}, Methods: []string{"DELETE"}, Blocked: true},
		{Hosts: []string{"api.anthropic.com"}, Paths: []string{"/private/**"}, Blocked: true},
	}}
	policy := []SecretRoute{{Hosts: []string{"api.anthropic.com"}}}
	if changed, err := sec.RestrictRoutes(policy); err != nil || !changed {
		t.Fatalf("first restriction: %v %v", changed, err)
	}
	// Persist/reload the shape, then repeat as refresh and capture do.
	raw, err := json.Marshal(sec)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded Secret
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatal(err)
	}
	if changed, err := reloaded.RestrictRoutes(policy); err != nil || changed {
		t.Fatalf("not idempotent: %v %v", changed, err)
	}
	for _, tc := range []struct {
		method, host, path string
		allow              bool
	}{
		{"GET", "api.anthropic.com", "/v1/messages", true},
		{"DELETE", "api.anthropic.com", "/v1/messages", false},
		{"GET", "api.anthropic.com", "/private/secret", false},
		{"GET", "evil.example", "/v1/messages", false},
	} {
		lists := append([][]SecretRoute{reloaded.Routes}, reloaded.RouteConstraints...)
		allowed, _ := secretroute.Decide(lists, secretroute.Request{Method: tc.method, Host: tc.host, URL: "https://" + tc.host + tc.path})
		if allowed != tc.allow {
			t.Fatalf("%+v: allow %v", tc, allowed)
		}
	}
	if changed, err := reloaded.RestrictRoutes(nil); err != nil || !changed {
		t.Fatalf("deny all: %v %v", changed, err)
	}
	if allowed, _ := secretroute.Decide(append([][]SecretRoute{reloaded.Routes}, reloaded.RouteConstraints...), secretroute.Request{Method: "GET", Host: "api.anthropic.com", URL: "https://api.anthropic.com/v1/messages"}); allowed {
		t.Fatal("empty constraint allowed a request")
	}
}
