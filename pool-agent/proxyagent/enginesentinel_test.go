package proxyagent

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/vm/pkg/machine"
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

// The engine registers, and calls with, only a key file the server placed a
// sentinel in: a real key there is neither written to SecretsFile nor sent.
func TestEngineSentinelRefusesAKeyFileThatHoldsNoSentinel(t *testing.T) {
	root := withTestRoot(t)
	sentinel, err := newEngineSentinel(root, testProjectID, testPoolID, "archil", []byte(`{"region":"aws-us-east-1"}`), "https://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	notSentinel := func(context.Context) (machine.Key, error) {
		return machine.Key{Driver: "archil", Secret: "key-AREALKEY"}, nil
	}
	if err := sentinel.Register(t.Context(), notSentinel); err == nil {
		t.Fatal("a key file holding no sentinel was registered")
	}
	if _, err := sentinel.Keys(notSentinel)(t.Context()); err == nil {
		t.Fatal("the engine called with a key file holding no sentinel")
	}
	if data, _ := os.ReadFile(root.ProxySecretsFile(testProjectID, testPoolID)); strings.Contains(string(data), "AREALKEY") {
		t.Fatalf("SecretsFile holds the key: %s", data)
	}
}

// A machine whose system store already trusts the proxy's MITM CA is refused:
// it would undo the CA's restriction to the hosts the key goes to.
func TestEngineRootsRefuseASystemStoreThatTrustsTheMITMCA(t *testing.T) {
	bundle, err := PrepareBundle(withTestRoot(t), testProjectID, testPoolID)
	if err != nil {
		t.Fatal(err)
	}
	clean := x509.NewCertPool()
	roots, err := engineRoots(clean, bundle, []string{"control.example.com"})
	if err != nil || roots == nil {
		t.Fatalf("engineRoots() over a clean store = %v", err)
	}
	mitm, err := x509.ParseCertificate(bundle.MITMCA.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	tainted := x509.NewCertPool()
	tainted.AddCert(mitm)
	if _, err := engineRoots(tainted, bundle, []string{"control.example.com"}); err == nil {
		t.Fatal("engineRoots() took a system store that trusts the MITM CA")
	}
}
