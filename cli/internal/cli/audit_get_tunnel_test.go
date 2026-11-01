package cli

import (
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// A passthrough tunnel's record says it was not intercepted, and what was seen
// from outside it (ADR 26-10-31-819 §4).
func TestHTTPAuditTunnelFieldsDescribeWhatWasSeenFromOutside(t *testing.T) {
	detail := &apimodel.HTTPAuditExchangeDetail{Tunnel: apiclientgen.NewOptHTTPAuditTunnel(apiclientgen.HTTPAuditTunnel{
		ID:                 "tun_1",
		Phase:              "close",
		Entry:              apiclientgen.NewOptString("*.corp.example"),
		TLS:                apiclientgen.NewOptBool(true),
		ServerName:         apiclientgen.NewOptString("other.example"),
		ServerNameMismatch: apiclientgen.NewOptBool(true),
		Alpn:               []string{"h2"},
		C2sBytes:           apiclientgen.NewOptInt64(10),
		S2cBytes:           apiclientgen.NewOptInt64(20),
		ClosedBy:           apiclientgen.NewOptString("client"),
	})}
	var rendered []string
	for _, field := range httpAuditTunnelFields(detail) {
		if field.value != "" {
			rendered = append(rendered, field.label+": "+field.value)
		}
	}
	got := strings.Join(rendered, "\n")
	for _, want := range []string{"close tun_1, not intercepted", "passthrough entry: *.corp.example", "server name other.example; which is not the CONNECT's host", "ALPN h2", "10 bytes sent, 20 received, ended by the client"} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered fields lack %q:\n%s", want, got)
		}
	}
	if fields := httpAuditTunnelFields(&apimodel.HTTPAuditExchangeDetail{}); fields != nil {
		t.Fatalf("a row that is no tunnel's rendered %v", fields)
	}
}
