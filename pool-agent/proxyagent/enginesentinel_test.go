package proxyagent

import (
	"encoding/json"
	"slices"
	"testing"

	_ "github.com/discobox-ai/vm/pkg/machine/boxd"
)

// A driver whose key is sent nowhere, or to hosts named only at run time, has
// no place a proxy route could hold a sentinel to.
func TestDriverKeySendsNeedAPlaceARouteCanName(t *testing.T) {
	if sends, err := DriverKeySends("fake", nil); err == nil || len(sends) != 0 {
		t.Errorf("fake's sends = %v, %v; want none: it sends its key nowhere", sends, err)
	}
	if _, err := DriverKeySends("boxd", json.RawMessage(`{"region":"fra"}`)); err == nil {
		t.Error("boxd's sends were taken, though it sends its key to hosts its service lists")
	}
	sends, err := DriverKeySends("archil", json.RawMessage(`{"region":"aws-us-east-1"}`))
	// With no baseUrl, archil sends its key to any region's control plane.
	if err != nil || len(sends) != 1 || !slices.Contains(sends[0].Hosts, "control.green.us-east-1.aws.prod.archil.com:443") || sends[0].Path != "/api/" || sends[0].Header != "Authorization" {
		t.Fatalf("archil's sends = %+v, %v; want the control planes' Authorization under /api/", sends, err)
	}
}
