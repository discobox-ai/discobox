package cli

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/adrg/xdg"

	"github.com/discobox-ai/discobox/endpoint"
)

// useTempServersFile points the registered servers at a directory of the
// test's own, so nothing reads or writes the machine's real configuration.
func useTempServersFile(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	xdg.Reload()
	t.Cleanup(xdg.Reload)
}

func registerForTest(t *testing.T, servers ...registeredServer) {
	t.Helper()
	if err := (serverRegistry{Servers: servers}).save(); err != nil {
		t.Fatalf("save registry: %v", err)
	}
}

func TestServerRegistryRoundTrips(t *testing.T) {
	useTempServersFile(t)

	empty, err := loadServerRegistry()
	if err != nil {
		t.Fatalf("loadServerRegistry() with no file error = %v", err)
	}
	if len(empty.Servers) != 0 {
		t.Fatalf("no file = %+v, want no servers", empty)
	}

	want := []registeredServer{{Name: "box", Address: "discobox://box.example.com"}, {Name: "lab", Address: "discobox://10.0.0.5:8443"}}
	registerForTest(t, want...)
	got, err := loadServerRegistry()
	if err != nil {
		t.Fatalf("loadServerRegistry() error = %v", err)
	}
	if !reflect.DeepEqual(got.Servers, want) {
		t.Fatalf("loadServerRegistry() = %+v, want %+v", got.Servers, want)
	}
}

// A registry that does not parse is what somebody wrote down; listing none of
// their servers would look like the servers had gone.
func TestServerRegistryRefusesACorruptFile(t *testing.T) {
	useTempServersFile(t)
	registerForTest(t)
	if err := os.WriteFile(serversFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadServerRegistry(); err == nil || !strings.Contains(err.Error(), serversFile()) {
		t.Fatalf("loadServerRegistry() error = %v, want it to name the file", err)
	}
}

func TestValidServerName(t *testing.T) {
	for _, name := range []string{"box", "box-2", "box.local", "lab_1", "0"} {
		if err := validServerName(name); err != nil {
			t.Fatalf("validServerName(%q) = %v", name, err)
		}
	}
	for _, name := range []string{"", "Box", "-box", "my box", "discobox://box", strings.Repeat("a", 64)} {
		if err := validServerName(name); err == nil {
			t.Fatalf("validServerName(%q) accepted it", name)
		}
	}
}

// A server's name is its hostname by default, which may be anything a
// hostname is; what it is registered under has to be a name --server takes.
func TestRegistrationName(t *testing.T) {
	for _, tc := range []struct{ offered, address, want string }{
		{"Workstation", "discobox://10.0.0.5:8443", "workstation"},
		{"Darren's MacBook Pro", "discobox://10.0.0.5:8443", "darren-s-macbook-pro"},
		{"box.local.", "discobox://10.0.0.5:8443", "box.local"},
		{"", "discobox://10.0.0.5:8443", "10.0.0.5"},
		{"!!!", "discobox://Box.Example.com", "box.example.com"},
		{"", "unix:///tmp/discobox/server.sock", "local"},
	} {
		if got := registrationName(tc.offered, tc.address); got != tc.want {
			t.Fatalf("registrationName(%q, %q) = %q, want %q", tc.offered, tc.address, got, tc.want)
		}
		if err := validServerName(registrationName(tc.offered, tc.address)); err != nil {
			t.Fatalf("registrationName(%q, %q) is not a valid name: %v", tc.offered, tc.address, err)
		}
	}
}

func TestUniqueServerName(t *testing.T) {
	reg := serverRegistry{Servers: []registeredServer{{Name: "box"}, {Name: "box-2"}}}
	if got := uniqueServerName(reg, "lab"); got != "lab" {
		t.Fatalf("uniqueServerName(lab) = %q, want it unchanged", got)
	}
	if got := uniqueServerName(reg, "box"); got != "box-3" {
		t.Fatalf("uniqueServerName(box) = %q, want box-3", got)
	}
	long := strings.Repeat("a", serverNameMaxLen)
	reg.Servers = append(reg.Servers, registeredServer{Name: long})
	if got := uniqueServerName(reg, long); len(got) > serverNameMaxLen || validServerName(got) != nil {
		t.Fatalf("uniqueServerName(long) = %q, want a valid name", got)
	}
}

// One server is one server however its address was written, which is what
// keeps a registered primary from being listed twice.
func TestServerKeyIsTheServerNotTheSpelling(t *testing.T) {
	var key [32]byte
	key[0] = 0xaa
	id, err := endpoint.IrohIDFromPublicKey(key[:])
	if err != nil {
		t.Fatal(err)
	}
	for _, same := range [][2]string{
		{"discobox://" + id.String(), "discobox://" + strings.ToUpper(strings.ReplaceAll(id.String(), "-", ""))},
		{"discobox://Box.Example.com:8443", "https://box.example.com:8443/"},
		{"discobox+http://127.0.0.1:8081", "http://127.0.0.1:8081"},
		{"discobox://box.example.com", "discobox://BOX.example.com"},
	} {
		if serverKey(same[0]) != serverKey(same[1]) {
			t.Fatalf("serverKey(%q) != serverKey(%q)", same[0], same[1])
		}
	}
	if serverKey("discobox://box.example.com") == serverKey("discobox://other.example.com") {
		t.Fatal("two servers share a key")
	}
}

func TestServerFlagTakesARegisteredName(t *testing.T) {
	useTempServersFile(t)
	registerForTest(t, registeredServer{Name: "lab", Address: "discobox://10.0.0.5:8443"})

	app := &App{serverURL: "lab"}
	if err := app.resolveServerName(); err != nil {
		t.Fatalf("resolveServerName() error = %v", err)
	}
	if app.serverURL != "discobox://10.0.0.5:8443" {
		t.Fatalf("serverURL = %q, want the registered address", app.serverURL)
	}

	// An address is left as it is: it has a scheme, and a name cannot.
	app = &App{serverURL: "unix:///tmp/discobox/server.sock"}
	if err := app.resolveServerName(); err != nil || app.serverURL != "unix:///tmp/discobox/server.sock" {
		t.Fatalf("resolveServerName() on an address = %q, %v", app.serverURL, err)
	}

	app = &App{serverURL: "nowhere"}
	if err := app.resolveServerName(); err == nil || !strings.Contains(err.Error(), "discobox servers") {
		t.Fatalf("resolveServerName() on an unknown name error = %v, want it to say where names come from", err)
	}
}

// A primary that is also registered is one server, listed once under the name
// it was registered as.
func TestServersListsARegisteredPrimaryOnce(t *testing.T) {
	useTempServersFile(t)
	registerForTest(t,
		registeredServer{Name: "lab", Address: "discobox://10.0.0.5:8443"},
		registeredServer{Name: "home", Address: "https://10.0.0.9:443"},
	)
	app := &App{serverURL: "https://10.0.0.5:8443"}
	set, err := app.servers()
	if err != nil {
		t.Fatalf("servers() error = %v", err)
	}
	if len(set) != 2 {
		t.Fatalf("servers() = %d servers, want the primary and one other", len(set))
	}
	if !set[0].primary || !set[0].registered || set[0].name != "lab" || set[0].app != app {
		t.Fatalf("primary = %+v, want the invocation itself, registered as lab", *set[0])
	}
	other := set[1]
	if other.primary || other.name != "home" || other.app.serverURL != "https://10.0.0.9:443" {
		t.Fatalf("second = %+v, want home", *other)
	}
	// A registered server is never started, works in its own default project,
	// and is not handed the token the primary was given.
	if other.app.autoStart != autoStartServerFalse || other.app.projectID != defaultProjectAlias || other.app.token != "" {
		t.Fatalf("registered server's app: autoStart %q, project %q, token %q", other.app.autoStart, other.app.projectID, other.app.token)
	}
}
