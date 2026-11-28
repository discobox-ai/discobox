package server

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/services"
)

// A service's ports go out in both shapes (ADR 26-11-24-806 §4): declaredPorts
// with each port's protocol, and ports with each number once, all an older
// reader knows.
func TestServiceReportsEachDeclaredPortWithItsProtocol(t *testing.T) {
	service := sandboxService(services.Service{Definition: services.Definition{
		ID:   "dns",
		Name: "dns",
		Ports: []services.Port{
			{Number: 53},
			{Number: 53, Protocol: "udp"},
			{Number: 8080, Protocol: "http"},
		},
	}})
	body, err := json.Marshal(&service)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Ports         []int64 `json:"ports"`
		DeclaredPorts []struct {
			Port     int64   `json:"port"`
			Protocol *string `json:"protocol"`
		} `json:"declaredPorts"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	if len(got.Ports) != 2 || got.Ports[0] != 53 || got.Ports[1] != 8080 {
		t.Errorf("ports = %v, want [53 8080]", got.Ports)
	}
	want := []string{"53:", "53:udp", "8080:http"}
	if len(got.DeclaredPorts) != len(want) {
		t.Fatalf("declaredPorts = %s, want %v", body, want)
	}
	for i, port := range got.DeclaredPorts {
		protocol := ""
		if port.Protocol != nil {
			protocol = *port.Protocol
		}
		if text := fmt.Sprintf("%d:%s", port.Port, protocol); text != want[i] {
			t.Errorf("declaredPorts[%d] = %s, want %s", i, text, want[i])
		}
	}
}
