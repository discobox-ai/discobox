package proxyagent

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"

	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/proxy/bridge"
)

// engineProxyURL is where the engine reaches the pool's proxy: on this
// machine, at ListenAddress's port, verified as ServerName.
const engineProxyURL = "https://127.0.0.1:17080"

// EngineSentinel is how a machine pool's engine calls its driver's service when
// the provisioning key file holds a sentinel for the driver's key (ADR
// 26-11-11-080 §9): every connection to a host the driver sends the key to
// goes through the pool's proxy, as the engine's own client
// (EngineClientID), and the proxy swaps the provider's key in at
// the place the driver declares it goes. The sentinel is registered with the
// proxy, held to a grant of those places alone.
//
// Every other connection the driver makes (Archil's process WebSocket, at a
// host each connection names) is dialed direct, never through the proxy: it
// carries no key, and its URL carries a credential the audit must not keep.
// The engine trusts the proxy's MITM CA beside the system's roots, restated
// with name constraints for the hosts the key goes to (ConstrainedMITMCA): a
// driver is given one set of roots, and a certificate the proxy's CA signs
// for any other host is believed by none of the engine's connections.
type EngineSentinel struct {
	root              layout.Root
	projectID, poolID string
	hosts             []string
	grants            []secretroute.Grant
	// RootCAs are what the engine trusts: the system's roots and the
	// pool proxy's MITM CA.
	RootCAs *x509.CertPool
	proxy   *bridge.Dialer
	direct  net.Dialer
	// registered is the sentinel last registered with the proxy.
	registerMu sync.Mutex
	registered string
}

// NewEngineSentinel reads where the pool's driver sends its key (disco-vm's
// SecretSends, under the provider's configuration the bootstrap carries), and
// issues the engine's client certificate.
func NewEngineSentinel(root layout.Root, projectID, poolID, driver string, driverConfig []byte) (*EngineSentinel, error) {
	return newEngineSentinel(root, projectID, poolID, driver, driverConfig, engineProxyURL)
}

func newEngineSentinel(root layout.Root, projectID, poolID, driver string, driverConfig []byte, proxyURL string) (*EngineSentinel, error) {
	sends, err := DriverKeySends(driver, driverConfig)
	if err != nil {
		return nil, err
	}
	routes, hosts, err := sendRoutes(sends)
	if err != nil {
		return nil, fmt.Errorf("the %s driver's key: %w", driver, err)
	}
	material, err := EnsureEngineMaterial(root, projectID, poolID)
	if err != nil {
		return nil, err
	}
	bundle, err := PrepareBundle(root, projectID, poolID)
	if err != nil {
		return nil, err
	}
	// The proxy's MITM CA, restated to vouch for the hosts the key goes to
	// alone (ADR 26-11-11-080 §9: the engine trusts the proxy's CA for those
	// hosts alone).
	mitm, err := bundle.ConstrainedMITMCA(hosts)
	if err != nil {
		return nil, err
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	roots.AddCert(mitm)
	dialer, err := bridge.NewDialer(bridge.DialConfig{
		URL:            proxyURL,
		ServerName:     ServerName,
		MTLSCAPath:     material.MTLSCAPath,
		ClientCertPath: material.ClientCertPath,
		ClientKeyPath:  material.ClientKeyPath,
	})
	if err != nil {
		return nil, fmt.Errorf("the pool proxy's dialer: %w", err)
	}
	k := &EngineSentinel{
		root:      root,
		projectID: projectID,
		poolID:    poolID,
		hosts:     hosts,
		grants:    []secretroute.Grant{{Lists: [][]secretroute.Route{routes}}},
		RootCAs:   roots,
		proxy:     dialer,
	}
	return k, nil
}

// DriverKeySends is where a driver sends its one secret, its key, under a
// configuration (disco-vm's SecretSends): the places a pool's proxy swaps a
// sentinel for the key at, and nowhere else (ADR 26-11-11-080 §9). A driver
// with no secret or more than one, one sent nowhere, or one sent to hosts its
// service names only at run time (Listed), which no route can name, has no
// such place, and takes no sentinel. The server decides by it whether a
// driver's pools hold a sentinel and which hosts it resolves the key for;
// the pool agent, the routes its engine's sentinel is held to.
func DriverKeySends(driver string, config []byte) ([]machine.SecretSend, error) {
	all, err := machine.SecretSends(driver, config)
	if err != nil {
		return nil, fmt.Errorf("where the %s driver sends its key: %w", driver, err)
	}
	if len(all) != 1 {
		return nil, fmt.Errorf("the %s driver takes %d secrets, and a sentinel stands for one key", driver, len(all))
	}
	for _, sends := range all {
		if len(sends) == 0 {
			return nil, fmt.Errorf("the %s driver declares no place it sends its key", driver)
		}
		for _, send := range sends {
			if send.Listed {
				return nil, fmt.Errorf("the %s driver sends its key to hosts its service names at run time, which no route can name", driver)
			}
		}
		return sends, nil
	}
	return nil, nil
}

// sendRoutes is the route list a sentinel is held to, one route per place the
// driver sends its key (disco-vm's SecretSend), and the hosts they name: its
// host names, its method, and its path, exact or, ending in "/", everything
// below it. A header send is the request's headers; a JSON field send is that
// body place, and nothing else.
func sendRoutes(sends []machine.SecretSend) ([]secretroute.Route, []string, error) {
	var routes []secretroute.Route
	var hosts []string
	for _, send := range sends {
		route := secretroute.Route{}
		for _, hostPort := range send.Hosts {
			host, _, err := net.SplitHostPort(hostPort)
			if err != nil {
				return nil, nil, fmt.Errorf("host %q: %w", hostPort, err)
			}
			host = strings.ToLower(host)
			route.Hosts = append(route.Hosts, host)
			if !slices.Contains(hosts, host) {
				hosts = append(hosts, host)
			}
		}
		if len(route.Hosts) == 0 {
			return nil, nil, fmt.Errorf("a place it is sent names no host")
		}
		if send.Method != "" {
			route.Methods = []string{strings.ToUpper(send.Method)}
		} else if send.Protocol == machine.SendGRPC {
			route.Methods = []string{"POST"}
		}
		switch {
		case send.Path == "" || send.Path == "/":
			route.Paths = []string{"/**"}
		case strings.HasSuffix(send.Path, "/"):
			route.Paths = []string{send.Path + "**"}
		default:
			route.Paths = []string{send.Path}
		}
		if send.JSONField != "" {
			pointer := "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(send.JSONField)
			route.Body = &secretroute.Body{Places: []string{pointer}}
		}
		normalized, err := secretroute.Normalize(route)
		if err != nil {
			return nil, nil, fmt.Errorf("a place it is sent: %w", err)
		}
		routes = append(routes, normalized)
	}
	return routes, hosts, nil
}

// Dial is the engine's machine.DialFunc: a connection to a host the key is
// sent to is a CONNECT through the pool's proxy; any other is direct.
func (k *EngineSentinel) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(k.hosts, strings.ToLower(strings.TrimSuffix(host, "."))) {
		return k.direct.DialContext(ctx, network, address)
	}
	// A certificate past its renewal point is reissued before the handshake
	// reads it.
	if _, err := EnsureEngineMaterial(k.root, k.projectID, k.poolID); err != nil {
		return nil, err
	}
	conn, err := k.proxy.Dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("reach the pool's proxy for %s: %w", address, err)
	}
	tunneled, err := connectThrough(ctx, conn, address)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tunneled, nil
}

// Register registers the sentinel source reads now with the pool's proxy.
// The agent calls it before the proxy unit starts, which reads SecretsFile
// as it starts, so the engine's first call through the proxy finds its
// sentinel there (Keys registers one that changes later, which the proxy's
// watcher takes up within moments).
func (k *EngineSentinel) Register(ctx context.Context, source machine.KeySource) error {
	key, err := source(ctx)
	if err != nil {
		return err
	}
	return k.register(key.Secret)
}

// Keys is source, with any sentinel it reads that is not the one registered
// with the pool's proxy registered before it is used: a pool whose record of
// its sentinel was lost may be placed another.
func (k *EngineSentinel) Keys(source machine.KeySource) machine.KeySource {
	return func(ctx context.Context) (machine.Key, error) {
		key, err := source(ctx)
		if err != nil {
			return key, err
		}
		if err := k.register(key.Secret); err != nil {
			return machine.Key{}, fmt.Errorf("register the provisioning key's sentinel with the pool's proxy: %w", err)
		}
		return key, nil
	}
}

func (k *EngineSentinel) register(sentinel string) error {
	k.registerMu.Lock()
	defer k.registerMu.Unlock()
	if sentinel == "" || sentinel == k.registered {
		return nil
	}
	if err := UpsertSandboxSentinels(k.root, k.projectID, k.poolID, EngineClientID, []string{sentinel}, nil, map[string][]secretroute.Grant{sentinel: k.grants}, nil); err != nil {
		return err
	}
	k.registered = sentinel
	return nil
}
