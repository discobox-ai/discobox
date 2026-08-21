package cli

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/go-faster/jx"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/portforward"
)

func sandboxWithPorts(t *testing.T, ports []apimodel.SandboxAgentListeningPort) apimodel.Sandbox {
	t.Helper()
	encoded, err := json.Marshal(ports)
	if err != nil {
		t.Fatalf("encode ports: %v", err)
	}
	var sandbox apimodel.Sandbox
	sandbox.Runtime.AgentStatus.SetTo(apiclientgen.SandboxRuntimeAgentStatus{"ports": jx.Raw(encoded)})
	return sandbox
}

func TestSandboxPortTargetsKeepsTheAddressToDial(t *testing.T) {
	sandbox := sandboxWithPorts(t, []apimodel.SandboxAgentListeningPort{
		{Port: 8080, Addresses: []string{"0.0.0.0"}, Protocol: "http"},
		{Port: 5432, Addresses: []string{"127.0.0.1"}, Protocol: "tcp"},
		{Port: 9000, Addresses: []string{"10.1.2.3"}, Protocol: "unknown"},
		{Port: 0, Addresses: []string{"0.0.0.0"}, Protocol: "tcp"},
	})
	want := []portforward.Target{
		{Host: "localhost", Port: 8080, Protocol: "http"},
		{Host: "localhost", Port: 5432, Protocol: "tcp"},
		{Host: "10.1.2.3", Port: 9000, Protocol: "unknown"},
	}
	if got := sandboxPortTargets(sandbox); !reflect.DeepEqual(got, want) {
		t.Fatalf("sandboxPortTargets = %#v, want %#v", got, want)
	}
}

func TestSandboxPortTargetsIsEmptyWithoutAReport(t *testing.T) {
	var sandbox apimodel.Sandbox
	if got := sandboxPortTargets(sandbox); len(got) != 0 {
		t.Fatalf("sandboxPortTargets = %#v, want none", got)
	}
	sandbox.Runtime.AgentStatus.SetTo(apiclientgen.SandboxRuntimeAgentStatus{"ports": jx.Raw(`"not a list"`)})
	if got := sandboxPortTargets(sandbox); len(got) != 0 {
		t.Fatalf("sandboxPortTargets on malformed ports = %#v, want none", got)
	}
}

// The listing rows are the same report with the address dropped, so the two
// never disagree about which ports a sandbox is serving.
func TestSandboxListeningPortsIsTheSameReport(t *testing.T) {
	sandbox := sandboxWithPorts(t, []apimodel.SandboxAgentListeningPort{
		{Port: 8080, Addresses: []string{"0.0.0.0"}, Protocol: "http"},
	})
	ports := sandboxListeningPorts(sandbox)
	if len(ports) != 1 || ports[0].Number != 8080 || ports[0].Protocol != "http" {
		t.Fatalf("sandboxListeningPorts = %#v", ports)
	}
}

// A loopback or wildcard bind dials the name, so both loopback families are
// tried: a listener bound only to ::1 refuses 127.0.0.1.
func TestDialHostForPortPrefersLoopback(t *testing.T) {
	for _, testCase := range []struct {
		addresses []string
		want      string
	}{
		{nil, "localhost"},
		{[]string{"0.0.0.0"}, "localhost"},
		{[]string{"::"}, "localhost"},
		{[]string{"::1"}, "localhost"},
		{[]string{"10.1.2.3", "127.0.0.1"}, "localhost"},
		{[]string{"10.1.2.3"}, "10.1.2.3"},
		{[]string{""}, "localhost"},
	} {
		if got := dialHostForPort(testCase.addresses); got != testCase.want {
			t.Errorf("dialHostForPort(%v) = %q, want %q", testCase.addresses, got, testCase.want)
		}
	}
}

func TestSandboxTCPWebSocketURL(t *testing.T) {
	for _, testCase := range []struct {
		baseURL string
		want    string
	}{
		{"http://127.0.0.1:8080", "ws://127.0.0.1:8080/api/projects/proj-1/sandboxes/sbx-1/tcp/attach?host=127.0.0.1&port=8080"},
		{"https://discobox.example/", "wss://discobox.example/api/projects/proj-1/sandboxes/sbx-1/tcp/attach?host=127.0.0.1&port=8080"},
		// A unix endpoint keeps its scheme-less host: the HTTP client dials the
		// socket regardless of what the URL says.
		{"http://localhost", "ws://localhost/api/projects/proj-1/sandboxes/sbx-1/tcp/attach?host=127.0.0.1&port=8080"},
	} {
		got, err := sandboxTCPWebSocketURL(testCase.baseURL, "proj-1", "sbx-1", "127.0.0.1", 8080)
		if err != nil {
			t.Fatalf("sandboxTCPWebSocketURL(%q): %v", testCase.baseURL, err)
		}
		if got != testCase.want {
			t.Errorf("sandboxTCPWebSocketURL(%q) = %q, want %q", testCase.baseURL, got, testCase.want)
		}
	}
}

func TestProxyTargetsForwardsEverythingReportedByDefault(t *testing.T) {
	reported := []portforward.Target{{Host: "127.0.0.1", Port: 8080}, {Host: "127.0.0.1", Port: 5432}}
	if got := proxyTargets(reported, nil); !reflect.DeepEqual(got, reported) {
		t.Fatalf("proxyTargets with no --port = %#v, want every reported port", got)
	}
}

// A named port is forwarded before the sandbox has reported it, and keeps what
// the report says about it once it has.
func TestProxyTargetsForwardsNamedPortsWhetherOrNotReported(t *testing.T) {
	reported := []portforward.Target{{Host: "10.1.2.3", Port: 5432, Protocol: "tcp"}}
	want := []portforward.Target{
		{Host: "10.1.2.3", Port: 5432, Protocol: "tcp"},
		{Host: "localhost", Port: 9999},
	}
	if got := proxyTargets(reported, []int{5432, 9999}); !reflect.DeepEqual(got, want) {
		t.Fatalf("proxyTargets = %#v, want %#v", got, want)
	}
	if got := proxyTargets(nil, []int{9999}); !reflect.DeepEqual(got, []portforward.Target{{Host: "localhost", Port: 9999}}) {
		t.Fatalf("proxyTargets with no listing = %#v", got)
	}
}
