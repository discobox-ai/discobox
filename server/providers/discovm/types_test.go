package discovm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// A driver reached at its URLs needs its secret, said rather than guessed,
// and its configuration is the driver's to refuse, naming the field and
// never its value. It may name the URL its pool machines reach the server
// at: without one the server opens a channel to each (ADR 26-10-30-875 §4).
// A held driver takes no control plane URL or base.
func TestValidateWhatADriverNeeds(t *testing.T) {
	isolateStateRoot(t)
	for _, tc := range []struct{ driver, config, want string }{
		{"boxd", `{}`, "driver.apiKey"},
		{"boxd", `{"driver":{"apiKey":"k"}}`, ""},
		{"boxd", `{"driver":"boxd"}`, "driver is the driver's configuration, a JSON object"},
		{"boxd", `{"driver":{"apiKey":"k","region":"fra","nope":1}}`, "nope"},
		{"boxd", `{"driver":{"apiKey":"k","tokenUrl":"http://app.example.com/t"}}`, "tokenUrl"},
		{"boxd", `{"driver":{"apiKey":"k"},"controlPlaneUrl":"unix:///run/discobox.sock"}`, "http or https"},
		{"boxd", `{"driver":{"apiKey":"k"},"controlPlaneUrl":"https://discobox.example.com","deliverProviderKey":true,"base":"ubuntu"}`, ""},
		{"archil", `{"driver":{"apiKey":"key-k","region":"aws-eu-west-1"}}`, ""},
		{"archil", `{"driver":{"apiKey":"key-k","region":"mars"}}`, "region"},
		{"fake", `{"controlPlaneUrl":"https://discobox.example.com"}`, "control plane URL"},
		{"fake", `{"base":"scratch"}`, "takes no base"},
		{"fake", `{"driver":{"key":"k"}}`, "takes no key"},
		{"fake", `{}`, ""},
	} {
		err := validate(tc.driver, json.RawMessage(tc.config))
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("validate(%s, %s) = %v, want %q", tc.driver, tc.config, err, tc.want)
		}
		if err != nil && (strings.Contains(err.Error(), "app.example.com") || strings.Count(err.Error(), tc.driver+":") > 1) {
			t.Errorf("validate(%s, %s) = %v, which names a value or repeats the driver", tc.driver, tc.config, err)
		}
	}
}

// A provider's configuration is the driver's: its fields under driver, its
// secret among them in the catalog but apart from what disco-vm is given,
// which is passed as the driver's key.
func TestDecodeSplitsTheDriversSecret(t *testing.T) {
	s, err := decode("boxd", []byte(`{"driver":{"apiKey":" bxd_k ","region":"fra"},"base":"img"}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.key != "bxd_k" || string(s.driverConfig) != `{"region":"fra"}` || s.Base != "img" {
		t.Fatalf("decode = key %q, driver %s, base %q; want the key apart from the region", s.key, s.driverConfig, s.Base)
	}
	key, err := providerKeys(s)(context.Background())
	if err != nil || key.Secret != "bxd_k" || key.Driver != "boxd" {
		t.Fatalf("boxd's key = %v, %v, want the configuration's", key, err)
	}
	none, err := decode("boxd", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := providerKeys(none)(context.Background()); err == nil || !strings.Contains(err.Error(), "driver.apiKey") {
		t.Fatalf("a provider with no key = %v, want an error naming driver.apiKey", err)
	}
}

// The catalog of each driver's type is read from disco-vm's registry: the
// driver's secret and fields under driver, every one fixed while the
// provider has pools, a Privileged one advanced and marked, and discobox's
// fields by the driver's kind (ADR 26-11-11-080 §§2, 3, 5).
func TestDefinitionIsTheDriversRegistry(t *testing.T) {
	isolateStateRoot(t)
	def := Definition("boxd")
	if def.Name != "boxd" || !strings.Contains(def.Description, "boxd") {
		t.Fatalf("Definition(boxd) = %q %q", def.Name, def.Description)
	}
	fields := map[string]sandbox.ProviderConfigField{}
	for _, f := range def.ConfigFields {
		fields[f.Key] = f
	}
	if f := fields["driver.apiKey"]; f.Type != sandbox.ProviderConfigFieldSecret || !f.Required || f.Immutable {
		t.Fatalf("driver.apiKey = %+v, want a required secret that rotates under pools", f)
	}
	if f := fields["driver.addr"]; !f.Privileged || !f.Advanced || !f.Immutable || f.Default != "boxd.sh:9443" {
		t.Fatalf("driver.addr = %+v, want a privileged, advanced, fixed field with its default", f)
	}
	if f := fields["driver.region"]; f.Privileged || !f.Immutable {
		t.Fatalf("driver.region = %+v, want a fixed field that is not privileged", f)
	}
	for _, key := range []string{"controlPlaneUrl", "base", "deliverProviderKey"} {
		if _, ok := fields[key]; !ok {
			t.Errorf("Definition(boxd) has no %s", key)
		}
	}
	if _, ok := fields["driver"]; ok {
		t.Error("Definition(boxd) lists driver itself")
	}
	held := map[string]bool{}
	for _, f := range Definition("fake").ConfigFields {
		held[f.Key] = true
		if f.Type == sandbox.ProviderConfigFieldSecret {
			t.Errorf("a held driver's catalog lists the secret %s, which it is never given", f.Key)
		}
	}
	if held["controlPlaneUrl"] || held["base"] {
		t.Fatalf("a held driver's catalog %v lists a field it refuses", held)
	}
}

// Every Privileged field of a linked driver is an endpoint its key is sent
// to, which ADR 26-11-11-080 §3 checks. A driver that gains another is linked
// again only once that field is checked: a host resource is never a project's
// to set, and a driver whose base is Privileged is not linked.
func TestPrivilegedFieldsAreTheCheckedEndpoints(t *testing.T) {
	want := map[string][]string{"archil": {"baseUrl"}, "boxd": {"addr", "tokenUrl"}, "hcs": nil}
	for _, name := range Types() {
		def, err := machine.DefinitionOf(name)
		if err != nil {
			t.Fatal(err)
		}
		if def.Base != nil && def.Base.Privileged {
			t.Errorf("%s's base is Privileged: a host resource", name)
		}
		var privileged []string
		for _, f := range def.ConfigFields {
			if f.Privileged {
				privileged = append(privileged, f.Key)
			}
			// disco-vm still decodes a Retired field into its ReplacedBy, and
			// the catalog lists it nowhere, so the providers service's
			// per-key checks (§§3–5) never see a write of it.
			if f.Retired {
				t.Errorf("%s has the retired field %s (into %q): compare the driver config as disco-vm normalizes it before linking it", name, f.Key, f.ReplacedBy)
			}
		}
		if fmt.Sprint(privileged) != fmt.Sprint(want[name]) {
			t.Errorf("%s's Privileged fields = %v, want %v: check the new one (ADR 26-11-11-080 §3) before linking it", name, privileged, want[name])
		}
	}
}

// A type whose name is taken fails its registration, never a user's server.
func TestRegisterRefusesATakenName(t *testing.T) {
	isolateStateRoot(t)
	manager := sandbox.NewProviderManager(nil)
	manager.RegisterProviderDefinition("boxd", sandbox.ProviderDefinition{Name: "boxd"})
	defer func() {
		if recover() == nil {
			t.Fatal("Register over a taken name did not panic")
		}
	}()
	Register(manager, nil, nil)
}

// Every connection the engine makes to the driver's service goes through
// the endpoint policy as it is dialed: a token URL on this host, which
// disco-vm allows a test and a write would refuse, reaches nothing, and the
// pool shows why rather than waiting on it (ADR 26-11-11-080 §3). The same
// endpoint carried from the server's environment is the operator's, and is
// dialed (§8), whatever its scheme.
func TestTheEnginesDialIsChecked(t *testing.T) {
	isolateStateRoot(t)
	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{}, 16)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			_ = conn.Close()
		}
	}()
	config := fmt.Sprintf(`{"driver":{"apiKey":"bxd_k","tokenUrl":"http://%s/token"}}`, listener.Addr())
	instance := &model.SandboxProviderInstance{ID: "prov_dial", ProjectID: "project-1", Type: "boxd", Config: model.RawJSON(config)}
	list := func() error {
		t.Helper()
		// As FactoryWithPoolManager opens it.
		s, err := decode("boxd", instance.Config)
		if err != nil {
			t.Fatal(err)
		}
		r, err := newRuntime(s, instance.ID, sandbox.EndpointPolicy{}.Dial(carriedValues(instance), nil))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err = r.engine.List(ctx)
		return err
	}
	err = list()
	if !errors.Is(err, sandbox.ErrEndpointRefused) {
		t.Fatalf("a call to a token URL on this host = %v, want the dial refused", err)
	}
	if unreachable(err) {
		t.Fatalf("a refused endpoint reads as unreachable, which a pool waits on unseen: %v", err)
	}
	if len(accepted) != 0 {
		t.Fatal("the refused endpoint was dialed")
	}
	instance.CarriedEndpoints = []string{"driver.tokenUrl"}
	err = list()
	if len(accepted) == 0 || errors.Is(err, sandbox.ErrEndpointRefused) {
		t.Fatalf("the carried endpoint was not dialed: %v", err)
	}
}
