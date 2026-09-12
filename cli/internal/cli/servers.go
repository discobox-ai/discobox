package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/adrg/xdg"
	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/endpoint"
	idpkg "github.com/discobox-ai/x/id"
)

// A client has one primary server — what --server names — and any number of
// registered ones beside it (ADR 0114 §3). Listings span all of them, and an
// operation on a discobox goes to the server it is on.

// serversFile is where the registered servers are kept: the user's config
// directory, beside the server's own server.yaml. It is configuration rather
// than state — what is in it is the user's decision, and means something to
// them — which is the line <state> is drawn on.
func serversFile() string {
	return filepath.Join(xdg.ConfigHome, "discobox", "servers.json")
}

// registeredServer is one server the user registered: the name it is listed
// under, which is theirs to choose, the address it is reached at, and its peer
// ID as the server gave it when it was registered — empty only for a server
// from before every server had one (ADR 0115).
type registeredServer struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	ID      string `json:"id,omitempty"`
}

type serverRegistry struct {
	Servers []registeredServer `json:"servers"`
}

// loadServerRegistry reads the registered servers; no file is no servers.
//
// A file that does not parse is an error, unlike the CLI's state files, which
// cost only a convenience when they go bad: this is what somebody wrote down,
// and listing none of their servers would look like the servers had gone.
func loadServerRegistry() (serverRegistry, error) {
	path := serversFile()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return serverRegistry{}, nil
	}
	if err != nil {
		return serverRegistry{}, err
	}
	var reg serverRegistry
	if err := json.Unmarshal(data, &reg); err != nil {
		return serverRegistry{}, fmt.Errorf("read %s: %w", path, err)
	}
	return reg, nil
}

func (r serverRegistry) save() error {
	return writeStateFile(serversFile(), r)
}

func (r serverRegistry) byName(name string) (int, bool) {
	for i, entry := range r.Servers {
		if entry.Name == name {
			return i, true
		}
	}
	return 0, false
}

// byServer finds the entry for the server at address, however either was
// written (serverKey), or with the peer ID peerID when that is known: one
// server reached over http and over iroh is two addresses and one peer.
func (r serverRegistry) byServer(address, peerID string) (int, bool) {
	key := serverKey(address)
	for i, entry := range r.Servers {
		if serverKey(entry.Address) == key || samePeer(entry.ID, peerID) {
			return i, true
		}
	}
	return 0, false
}

// samePeer reports whether two written peer IDs are one, however each was
// spelled. An empty one is no peer and matches nothing.
func samePeer(a, b string) bool {
	a, b = endpoint.NormalizePeerID(a), endpoint.NormalizePeerID(b)
	return a != "" && a == b
}

// peerID is the server's peer ID in its written form, from its address when
// the address names one and from the server otherwise (serverIdentity); empty
// when it has none, or did not say in time. It is what a registration records.
func (a *App) peerID(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, serverInfoTimeout)
	defer cancel()
	return a.serverIdentity(ctx).PeerID
}

// serverNameMaxLen is a DNS label's length: a name is something typed, and a
// server's hostname is what it usually starts from.
const serverNameMaxLen = 63

// validServerName reports why name cannot name a registered server. A name is
// what --server takes in place of an address, so it can never contain the
// "://" every address has, and it is kept to what a shell needs no quoting for.
func validServerName(name string) error {
	if name == "" {
		return fmt.Errorf("a server name is required")
	}
	if len(name) > serverNameMaxLen {
		return fmt.Errorf("server name %q is longer than %d characters", name, serverNameMaxLen)
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '.' || r == '_') && i > 0:
		default:
			return fmt.Errorf("server name %q may use lowercase letters and digits, and . _ - after the first character", name)
		}
	}
	return nil
}

// registrationName is the name a server is registered under when nobody chose
// one: the name it offers (ADR 0114 §2), made into one a name may be. A server
// that offers none, or none that survives, is named after its address.
func registrationName(offered, address string) string {
	if name := sanitizeServerName(offered); name != "" {
		return name
	}
	if name := sanitizeServerName(addressLabel(address)); name != "" {
		return name
	}
	return "server"
}

func sanitizeServerName(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-._")
	if len(name) > serverNameMaxLen {
		name = strings.TrimRight(name[:serverNameMaxLen], "-._")
	}
	return name
}

// uniqueServerName is base, or base with the first -2, -3… nothing registered
// is called yet.
func uniqueServerName(reg serverRegistry, base string) string {
	if _, taken := reg.byName(base); !taken {
		return base
	}
	for n := 2; ; n++ {
		suffix := fmt.Sprintf("-%d", n)
		candidate := base
		if len(candidate)+len(suffix) > serverNameMaxLen {
			candidate = strings.TrimRight(candidate[:serverNameMaxLen-len(suffix)], "-._")
		}
		candidate += suffix
		if _, taken := reg.byName(candidate); !taken {
			return candidate
		}
	}
}

// addressLabel is what a server is called when nothing names it: the part of
// its address a person would recognize it by.
func addressLabel(address string) string {
	parsed, err := endpoint.Parse(address)
	if err != nil {
		return strings.TrimSpace(address)
	}
	switch parsed.Scheme {
	case "unix", "npipe":
		return "local"
	case "iroh":
		if id, err := parsed.IrohID(); err == nil {
			return id.Short()
		}
	case endpoint.SchemeDiscobox:
		return parsed.Name
	case "http", "https":
		if u, err := url.Parse(parsed.Value); err == nil {
			return u.Hostname()
		}
	}
	return strings.TrimSpace(address)
}

// addressPeerID is the peer an address names, in its written form, or empty
// for an address that names its server some other way. It is what the primary
// can be compared against a registration by without asking anybody.
func addressPeerID(address string) string {
	parsed, err := endpoint.Parse(address)
	if err != nil || parsed.Scheme != "iroh" {
		return ""
	}
	id, err := parsed.IrohID()
	if err != nil {
		return ""
	}
	return id.String()
}

// serverKey is what makes two addresses one server however each was written:
// the endpoint they parse to. A peer is its ID whatever its dashes and case,
// an https server its URL, a socket its path.
func serverKey(address string) string {
	parsed, err := endpoint.Parse(address)
	if err != nil {
		return strings.TrimSpace(address)
	}
	value := parsed.Value
	if parsed.Scheme != "unix" && parsed.Scheme != "npipe" {
		value = strings.ToLower(strings.TrimRight(value, "/"))
	}
	return parsed.Scheme + " " + value
}

// resolveServerName lets --server name a registered server (ADR 0114 §3). An
// address always has a scheme and a registered name never can, so which one
// was written is never a guess.
func (a *App) resolveServerName() error {
	value := strings.TrimSpace(a.serverURL)
	if value == "" || strings.Contains(value, "://") {
		return nil
	}
	reg, err := loadServerRegistry()
	if err != nil {
		return err
	}
	i, ok := reg.byName(value)
	if !ok {
		return fmt.Errorf("--server %q is neither an address nor a registered server; `discobox servers` lists the registered ones", value)
	}
	a.serverURL = reg.Servers[i].Address
	return nil
}

// server is one server this invocation lists discoboxes from: the primary, or
// one the user registered.
type server struct {
	// name is what the server is listed as: its registered name, or for a
	// primary nobody registered, the name it offers once it has been asked,
	// and its address until then.
	name       string
	address    string
	primary    bool
	registered bool
	// id is the server's peer ID as registered, or as a primary nobody
	// registered gave it when asked; empty when unknown or it has none.
	id string
	// app is this invocation aimed at the server. Every path a command takes
	// to "the server" reads the App it runs on — the API client, the git
	// transport, the terminals, the ssh bridge — so aiming one is all it takes
	// to act on a discobox there.
	app *App
}

// servers is every server this invocation lists: the primary first, then the
// registered ones in the order they were registered, with a primary that is
// also registered listed once, under its registered name. It is read once per
// invocation, so every listing in it agrees on which servers there are.
func (a *App) servers() ([]*server, error) {
	a.serversOnce.Do(func() {
		a.serverSet, a.serverSetErr = a.loadServers()
	})
	return a.serverSet, a.serverSetErr
}

func (a *App) loadServers() ([]*server, error) {
	reg, err := loadServerRegistry()
	if err != nil {
		return nil, err
	}
	primary := &server{name: addressLabel(a.serverURL), address: a.serverURL, primary: true, app: a}
	set := []*server{primary}
	primaryKey := serverKey(a.serverURL)
	// The peer the primary's address names, when it names one: an address and a
	// peer ID are two spellings of one server (ADR 0114 §3), and reading it off
	// the address costs no round trip. A primary reached some other way has
	// none to compare until something asks it.
	primaryPeer := addressPeerID(a.serverURL)
	for _, entry := range reg.Servers {
		if serverKey(entry.Address) == primaryKey || samePeer(entry.ID, primaryPeer) {
			primary.name, primary.registered, primary.id = entry.Name, true, entry.ID
			continue
		}
		set = append(set, &server{name: entry.Name, address: entry.Address, registered: true, id: entry.ID, app: a.forServer(entry.Address)})
	}
	return set, nil
}

// serverNamed is the server in set listed as name. Callers pass the registered
// servers: their names are unique, and the primary's may be anything it offers.
func serverNamed(set []*server, name string) (*server, bool) {
	for _, s := range set {
		if s.name == name {
			return s, true
		}
	}
	return nil, false
}

// forServer is this invocation aimed at another server: the same transport
// settings, source directory and output, a different --server.
//
// It is never started — only the primary may be (ADR 0114 §3) — it works in
// the server's default project, since a project ID names a project on one
// server only, and it does not carry --token, which was given for the primary.
func (a *App) forServer(address string) *App {
	return &App{
		serverURL:     address,
		irohRelayURLs: a.irohRelayURLs,
		irohLogLevel:  a.irohLogLevel,
		projectID:     defaultProjectAlias,
		source:        a.source,
		output:        a.output,
		quiet:         a.quiet,
		debug:         a.debug,
		autoStart:     autoStartServerFalse,
		errOut:        a.errOut,
		leaderKey:     a.leaderKey,
	}
}

// serverInfoTimeout bounds asking a server what it is called. The name is a
// label, so a server slow to give one is named after its address rather than
// waited on.
const serverInfoTimeout = 5 * time.Second

// offeredName is the name a server offers for itself (ADR 0114 §2), or empty
// when it offers none: no hostname, a version that predates GET /server, or
// no answer in time. Nothing depends on it but a default.
func offeredName(ctx context.Context, client *apiclientgen.Client) string {
	ctx, cancel := context.WithTimeout(ctx, serverInfoTimeout)
	defer cancel()
	res, err := client.GetServerInfo(ctx)
	if err != nil {
		return ""
	}
	info, err := expectResponse[apimodel.ServerInfo](res)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(info.Name.Or(""))
}

// registeredServerTimeout bounds how long anything that spans servers waits on
// a registered one (ADR 0114 §4). The primary is waited on as it always was; a
// registered server that has not answered by then is reported and left out,
// rather than holding every other server's discoboxes back with it.
const registeredServerTimeout = 10 * time.Second

// bounded is ctx limited to registeredServerTimeout for a server that is not
// the primary.
func (s *server) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return s.boundedBy(ctx, registeredServerTimeout)
}

// boundedBy is bounded with a limit of its own, for work that is not a
// listing: the primary is never bounded here, since a command waits on its own
// server as long as it always did.
func (s *server) boundedBy(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	if s.primary {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, limit)
}

// serverSandbox is a discobox and the server it was listed from.
type serverSandbox struct {
	server  *server
	sandbox apimodel.Sandbox
}

// unansweredServer is a registered server a listing asked and did not hear
// from, whose discoboxes are therefore missing from it rather than gone.
type unansweredServer struct {
	server *server
	err    error
}

// listEveryServer is `discobox ls` across every server: the primary's
// discoboxes in --project, each registered server's in its default project,
// asked concurrently; all is `ls --all`. The primary failing fails the
// listing, as it always did. A registered server failing is returned as
// unreachable beside everything the others listed.
//
// A discobox listed by two servers is one server registered under two
// addresses — IDs are random, so it cannot be two discoboxes — and is listed
// once, from whichever comes first: the primary, or the one registered first.
func (a *App) listEveryServer(ctx context.Context, all bool) ([]serverSandbox, []unansweredServer, error) {
	set, err := a.servers()
	if err != nil {
		return nil, nil, err
	}
	type result struct {
		sandboxes []apimodel.Sandbox
		err       error
	}
	results := make([]result, len(set))
	var wg sync.WaitGroup
	for i, s := range set {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i].sandboxes, results[i].err = s.listSandboxes(ctx, all, len(set) > 1)
		}()
	}
	wg.Wait()

	var listed []serverSandbox
	var unreachable []unansweredServer
	seen := map[string]bool{}
	for i, s := range set {
		if err := results[i].err; err != nil {
			if s.primary {
				return nil, nil, err
			}
			unreachable = append(unreachable, unansweredServer{server: s, err: err})
			continue
		}
		for _, sandbox := range results[i].sandboxes {
			if seen[sandbox.ID] {
				continue
			}
			seen[sandbox.ID] = true
			listed = append(listed, serverSandbox{server: s, sandbox: sandbox})
		}
	}
	return listed, unreachable, nil
}

// listSandboxes is one server's part of a listing. named asks a primary
// nobody registered what it is called, which is only worth a round trip once
// there is more than one server to tell apart.
func (s *server) listSandboxes(ctx context.Context, all, named bool) ([]apimodel.Sandbox, error) {
	ctx, cancel := s.bounded(ctx)
	defer cancel()
	if _, err := s.app.resolveServerAddress(ctx); err != nil {
		return nil, err
	}
	projectID, err := s.app.projectIDValue()
	if err != nil {
		return nil, err
	}
	client, err := s.app.apiClient()
	if err != nil {
		return nil, err
	}
	sandboxes, err := s.app.listProjectSandboxes(ctx, client, projectID, all)
	if err != nil {
		return nil, err
	}
	if named && s.primary && !s.registered {
		if name := offeredName(ctx, client); name != "" {
			s.name = name
		}
	}
	return sandboxes, nil
}

// findSandbox looks for a discobox by ID, or by a prefix unique among this
// server's, the way `discobox attach` takes one. found is false when the
// server has no such discobox, which is a different answer from not being
// able to ask.
func (s *server) findSandbox(ctx context.Context, value string) (projectID, sandboxID string, client *apiclientgen.Client, found bool, err error) {
	ctx, cancel := s.bounded(ctx)
	defer cancel()
	if _, err := s.app.resolveServerAddress(ctx); err != nil {
		return "", "", nil, false, err
	}
	projectID, err = s.app.projectIDValue()
	if err != nil {
		return "", "", nil, false, err
	}
	client, err = s.app.apiClient()
	if err != nil {
		return "", "", nil, false, err
	}
	res, err := client.ListSandboxes(ctx, apiclientgen.ListSandboxesParams{ProjectId: projectID})
	if err != nil {
		return "", "", nil, false, err
	}
	body, err := expectResponse[apimodel.ListSandboxesBody](res)
	if err != nil {
		return "", "", nil, false, err
	}
	ids := make([]string, 0, len(body.GetSandboxes()))
	for _, sandbox := range body.GetSandboxes() {
		ids = append(ids, sandbox.ID)
	}
	matches := matchSandboxIDs(value, ids)
	switch len(matches) {
	case 0:
		return projectID, "", client, false, nil
	case 1:
		return projectID, matches[0], client, true, nil
	default:
		return "", "", nil, false, fmt.Errorf("short discobox ID %q is ambiguous on %s; matches %s", value, s.name, strings.Join(matches, ", "))
	}
}

// matchSandboxIDs is what value names among ids: itself when it is a whole ID
// or not shaped like a prefix, and every ID it is a unique prefix of
// otherwise — resolveShortID's reading, answering "none" rather than failing.
func matchSandboxIDs(value string, ids []string) []string {
	if isResolvableShortID(value) {
		return idpkg.ResolveShort(value, ids)
	}
	for _, id := range ids {
		if id == value {
			return []string{id}
		}
	}
	return nil
}

// findOnEveryServer resolves a discobox ID across servers (ADR 0114 §4): the
// primary first, as with one server, and the registered servers only when the
// primary has no such discobox, so an ID copied from `discobox ls` works
// whichever server listed it.
func (a *App) findOnEveryServer(ctx context.Context, set []*server, value string) (*App, string, string, *apiclientgen.Client, error) {
	value, err := parseIDArg(value, "discobox ID")
	if err != nil {
		return nil, "", "", nil, err
	}
	projectID, sandboxID, client, found, err := set[0].findSandbox(ctx, value)
	if err != nil {
		return nil, "", "", nil, err
	}
	if found {
		return a, projectID, sandboxID, client, nil
	}

	type hit struct {
		server             *server
		projectID, sandbox string
		client             *apiclientgen.Client
	}
	var (
		mu     sync.Mutex
		hits   []hit
		silent []string
		wg     sync.WaitGroup
	)
	for _, s := range set[1:] {
		wg.Add(1)
		go func() {
			defer wg.Done()
			projectID, sandboxID, client, found, err := s.findSandbox(ctx, value)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				silent = append(silent, s.name)
			case found:
				hits = append(hits, hit{server: s, projectID: projectID, sandbox: sandboxID, client: client})
			}
		}()
	}
	wg.Wait()
	switch len(hits) {
	case 1:
		return hits[0].server.app, hits[0].projectID, hits[0].sandbox, hits[0].client, nil
	case 0:
		if len(silent) > 0 {
			return nil, "", "", nil, fmt.Errorf("no discobox matches %q on any server that answered; %s did not", value, strings.Join(silent, ", "))
		}
		return nil, "", "", nil, fmt.Errorf("no discobox matches %q on any server", value)
	default:
		names := make([]string, 0, len(hits))
		for _, h := range hits {
			names = append(names, h.server.name)
		}
		return nil, "", "", nil, fmt.Errorf("%q matches a discobox on more than one server (%s); name it by its address, discobox://<server>/<discobox>", value, strings.Join(names, ", "))
	}
}

// selectAddressedSandbox resolves a discobox's address: the server it names,
// and the discobox on it. A server reached this way that is neither the
// primary nor registered is registered, once the discobox has been found on
// it (ADR 0114 §6) — so a mistyped address leaves nothing behind.
func (a *App) selectAddressedSandbox(cmd *cobra.Command, address endpoint.SandboxAddress) (*App, string, string, *apiclientgen.Client, error) {
	ctx := cmd.Context()
	set, err := a.servers()
	if err != nil {
		return nil, "", "", nil, err
	}
	target := &server{name: addressLabel(address.Server), address: address.Server, app: a.forServer(address.Server)}
	key := serverKey(address.Server)
	for _, s := range set {
		if serverKey(s.address) == key {
			target = s
			break
		}
	}
	projectID, sandboxID, client, found, err := target.findSandbox(ctx, address.Sandbox)
	if err != nil {
		return nil, "", "", nil, err
	}
	if !found {
		return nil, "", "", nil, fmt.Errorf("%s has no discobox %q", address.Server, address.Sandbox)
	}
	if !target.primary && !target.registered {
		note := printedNotes(cmd.ErrOrStderr())
		// Registering is the convenience; the discobox is what was asked for,
		// and a config directory that cannot be written is no reason to
		// refuse it.
		if name, added, err := registerServer(ctx, target, client); err != nil {
			note("Could not register %s: %v", address.Server, err)
		} else if added {
			note("Registered server %s at %s; `discobox ls` lists its discoboxes now", name, address.Server)
		}
	}
	return target.app, projectID, sandboxID, client, nil
}

// registerServer adds s to the registered servers under the name it offers,
// made unique among the ones registered already, with the peer ID it gives.
// added is false when the server is registered already — by another command
// first, or under another address carrying the same peer ID.
func registerServer(ctx context.Context, s *server, client *apiclientgen.Client) (name string, added bool, err error) {
	offered := offeredName(ctx, client)
	peerID := s.app.peerID(ctx)
	reg, err := loadServerRegistry()
	if err != nil {
		return "", false, err
	}
	if i, ok := reg.byServer(s.address, peerID); ok {
		s.name, s.registered = reg.Servers[i].Name, true
		return s.name, false, nil
	}
	name = uniqueServerName(reg, registrationName(offered, s.address))
	s.id = peerID
	reg.Servers = append(reg.Servers, registeredServer{Name: name, Address: s.address, ID: peerID})
	if err := reg.save(); err != nil {
		return "", false, err
	}
	s.name, s.registered = name, true
	return name, true, nil
}
