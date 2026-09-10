package endpoint

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	iroh "github.com/discobox-ai/iroh-go"

	"github.com/discobox-ai/discobox/health"
)

// Reaching a Discobox server is a stack, and an error from the top of it names
// only the top. `dial iroh endpoint d1-dtztd73: iroh: connect failed` is the
// same sentence whether the native library never loaded, the machine cannot
// reach a relay, the peer ID names a server that is switched off, or the peer
// is running and simply does not admit this client — four different problems
// with four different fixes.
//
// [Diagnose] takes the stack apart and reports each layer separately, in the
// order a connection passes through them. It is what `discobox status` prints,
// and it is deliberately a package-level function on the transport rather than
// a CLI-local helper: the layers it reports are this package's, and only this
// package can see the ones below `HTTPClient`.

// DiagnosisStatus is how one layer answered.
type DiagnosisStatus string

const (
	// DiagnosisOK means the layer worked.
	DiagnosisOK DiagnosisStatus = "ok"
	// DiagnosisWarn means the layer failed and the next one is still worth
	// trying — a relay that cannot be reached, when the peer may yet be
	// reachable directly.
	DiagnosisWarn DiagnosisStatus = "warn"
	// DiagnosisFailed means the layer failed and nothing above it can work.
	DiagnosisFailed DiagnosisStatus = "failed"
	// DiagnosisSkipped means the layer was not reached, or does not apply to
	// this endpoint.
	DiagnosisSkipped DiagnosisStatus = "skipped"
	// DiagnosisUnknown means the layer could not be decided either way. It is
	// its own answer rather than a failure: reporting "this peer is not
	// admitted" because the server never replied would send an operator to
	// enroll an ID that is already enrolled.
	DiagnosisUnknown DiagnosisStatus = "unknown"
)

// The layers, in the order a connection passes through them. They are strings
// rather than an enum because they are printed and serialized far more often
// than they are compared, and a caller adding one of its own — the CLI adds an
// authenticated API call above the transport — should not have to extend this
// package to do it.
const (
	// DiagnosisLayerAddress is parsing --server: the peer ID, or the socket
	// path, that everything else is about.
	DiagnosisLayerAddress = "address"
	// DiagnosisLayerRuntime is the native iroh library this build loads.
	DiagnosisLayerRuntime = "runtime"
	// DiagnosisLayerIdentity is this machine's own peer ID, the one a server
	// enrolls.
	DiagnosisLayerIdentity = "identity"
	// DiagnosisLayerBind is the local UDP socket.
	DiagnosisLayerBind = "bind"
	// DiagnosisLayerRelay is whether *this machine* reaches a relay server,
	// which is what makes a peer findable across networks.
	//
	// It answers for this end only. A green relay row above a failed connect
	// is the common shape of "the server is the one that is missing", and a
	// reader who takes it for the peer's relay reads it as a contradiction.
	DiagnosisLayerRelay = "relay"
	// DiagnosisLayerConnect is the QUIC handshake with the peer, which proves
	// its identity and nothing about whether it will admit us.
	DiagnosisLayerConnect = "connect"
	// DiagnosisLayerStream is a bidirectional stream on that connection: the
	// net.Conn the HTTP client is written against.
	DiagnosisLayerStream = "stream"
	// DiagnosisLayerAdmission is whether the server let this peer stay. A
	// server refuses by closing an accepted connection with a reason, so this
	// is answered by the first exchange rather than by the handshake.
	DiagnosisLayerAdmission = "admission"
	// DiagnosisLayerServer is the server's own readiness, read from /healthz.
	DiagnosisLayerServer = "server"
	// DiagnosisLayerRoute is how the connection actually reaches the peer:
	// straight to its socket, or through a relay, and at what address.
	//
	// It is last because its answer settles last. iroh opens a connection on a
	// relay and moves it onto a direct path when hole punching succeeds, so a
	// route read at the handshake reports where the connection started rather
	// than where it ended up. Reading it once the connection has carried a full
	// request attempt is what makes it the route traffic is taking — which is
	// also true of the peer that accepted the connection and then said nothing,
	// so that is reported rather than skipped.
	DiagnosisLayerRoute = "route"
)

// DiagnosisStep is one layer's answer.
type DiagnosisStep struct {
	Layer   string          `json:"layer"`
	Status  DiagnosisStatus `json:"status"`
	Summary string          `json:"summary,omitempty"`
	// Detail is what the layer learned, one fact per line: the sockets it
	// bound, the addresses it dialed, the version the server reported.
	Detail []string `json:"detail,omitempty"`
	// Hint is what to do about a failure, in the reader's terms. It is set
	// only when the layer failed, because a hint on a working layer is noise.
	Hint string `json:"hint,omitempty"`
	// Error is the underlying error's text, kept beside the summary so a bug
	// report carries what the transport actually said.
	Error string `json:"error,omitempty"`
	// DurationMS is how long the layer took. Milliseconds because that is the
	// resolution any of this is worth reading at.
	DurationMS int64 `json:"durationMs"`
}

// Diagnosis is the whole stack's answer, in layer order.
type Diagnosis struct {
	// Endpoint is the address that was diagnosed, as it was written.
	Endpoint string `json:"endpoint"`
	// Scheme is what it resolved to, so a discobox:// address reports the
	// transport it names.
	Scheme string `json:"scheme,omitempty"`
	// Transport describes that scheme in a phrase a reader can act on.
	Transport string `json:"transport,omitempty"`
	// ServerStatus is what the server said about itself, from [health.Status]:
	// "ready", or "starting" for one that has bound its listeners and is still
	// initializing. Empty when no server answered.
	//
	// It is carried separately from the server step's summary because a caller
	// above this package branches on it — a server that is starting answers
	// every path with 503, so there is no point asking it anything else — and
	// branching on rendered text is how that goes wrong later.
	ServerStatus string          `json:"serverStatus,omitempty"`
	Steps        []DiagnosisStep `json:"steps"`
}

// OK reports whether every layer that was reached worked.
func (d Diagnosis) OK() bool {
	return d.FirstFailure() == nil
}

// FirstFailure is the lowest layer that failed, which is the one to fix. A
// warning is not a failure: it is a layer that failed without stopping the one
// above it.
func (d Diagnosis) FirstFailure() *DiagnosisStep {
	for i, step := range d.Steps {
		if step.Status == DiagnosisFailed {
			return &d.Steps[i]
		}
	}
	return nil
}

// DiagnoseOptions bounds the waits. Every one of them has a default, so the
// zero value is the right thing to pass.
type DiagnoseOptions struct {
	// RelayTimeout bounds waiting for this endpoint to reach a relay. It is
	// short because a machine that can reach one usually does so in well under
	// a second, and a machine that cannot is what this is trying to report.
	RelayTimeout time.Duration
	// ConnectTimeout bounds the handshake with the peer, including whatever
	// discovery and hole punching it takes to find one.
	ConnectTimeout time.Duration
	// RequestTimeout bounds the health request once a stream exists.
	RequestTimeout time.Duration
}

const (
	defaultDiagnoseRelayTimeout   = 5 * time.Second
	defaultDiagnoseConnectTimeout = 20 * time.Second
	defaultDiagnoseRequestTimeout = 15 * time.Second
	// diagnoseCloseWait bounds reading a refused connection's close reason.
	// It is only paid on a failure, and only until the reason arrives.
	diagnoseCloseWait = 3 * time.Second
	// diagnoseResolveWait bounds reading back what a peer ID resolved to. It
	// is a local lookup in the endpoint's own address book, so it is short:
	// this runs after a dial has already spent its whole budget failing, and
	// a report that hangs afterwards is worse than one without this line.
	diagnoseResolveWait = 2 * time.Second
	// diagnoseUserAgent identifies these probes in a server's access log, so a
	// health request that arrived from `discobox status` is not mistaken for a
	// client that is about to do something.
	diagnoseUserAgent = "discobox-status (health probe)"
)

func (o DiagnoseOptions) withDefaults() DiagnoseOptions {
	if o.RelayTimeout <= 0 {
		o.RelayTimeout = defaultDiagnoseRelayTimeout
	}
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = defaultDiagnoseConnectTimeout
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = defaultDiagnoseRequestTimeout
	}
	return o
}

// Diagnose reports, layer by layer, how far a caller gets toward the server at
// raw and where it stops.
//
// An iroh endpoint must already be configured — [ConfigureIroh] installs the
// identity, the relays and the admission policy — because the identity a
// diagnosis reports has to be the one an ordinary command would connect with.
// Binding a second endpoint here would report a peer ID nobody enrolled.
func Diagnose(ctx context.Context, raw string, opts DiagnoseOptions) Diagnosis {
	opts = opts.withDefaults()
	diagnosis := &Diagnosis{Endpoint: raw}

	started := time.Now()
	parsed, err := Parse(raw)
	if err != nil {
		diagnosis.fail(DiagnosisLayerAddress, started, "this address cannot be read", err, unreadableAddressHint(raw))
		return *diagnosis
	}
	diagnosis.Scheme = parsed.Scheme
	diagnosis.Transport = transportDescription(parsed)

	if parsed.Scheme == "iroh" {
		// The address is checked before the identity, because an address that
		// names no peer is wrong whether or not this process has one, and
		// reporting the identity would send the reader to the wrong thing.
		if bad := irohAddressFailure(parsed, started); bad != nil {
			diagnosis.add(*bad)
			return *diagnosis
		}
		configured, configErr := defaultIrohEndpoint()
		if configErr != nil {
			// The same shape every other failure produces: the layer that
			// worked, the one that failed, and the ones never reached. A
			// report that named only the failure would be the one thing
			// skipRest exists to prevent.
			peer, _ := parsed.IrohID()
			diagnosis.add(irohAddressStep(parsed, peer, started))
			diagnosis.skip(DiagnosisLayerRuntime, "not reached")
			diagnosis.fail(DiagnosisLayerIdentity, time.Time{}, "this process has no peer identity", configErr,
				"Only a command that dials an iroh address installs one. This is a bug if the address above is one.")
			diagnosis.skipRest(DiagnosisLayerBind, DiagnosisLayerRelay, DiagnosisLayerConnect,
				DiagnosisLayerStream, DiagnosisLayerAdmission, DiagnosisLayerServer, DiagnosisLayerRoute)
			return *diagnosis
		}
		return configured.Diagnose(ctx, raw, opts)
	}

	diagnosis.ok(DiagnosisLayerAddress, started, parsed.Value)
	diagnoseDialable(ctx, diagnosis, parsed, opts)
	return *diagnosis
}

// Diagnose is [Diagnose] from this endpoint rather than from the process
// default, for a caller that holds its own — two peers on one host, which is
// what a test of the layers themselves needs.
func (e *IrohEndpoint) Diagnose(ctx context.Context, raw string, opts DiagnoseOptions) Diagnosis {
	opts = opts.withDefaults()
	diagnosis := &Diagnosis{Endpoint: raw}

	started := time.Now()
	parsed, err := Parse(raw)
	if err != nil {
		diagnosis.fail(DiagnosisLayerAddress, started, "this address cannot be read", err, unreadableAddressHint(raw))
		return *diagnosis
	}
	diagnosis.Scheme = parsed.Scheme
	diagnosis.Transport = transportDescription(parsed)
	if parsed.Scheme != "iroh" {
		diagnosis.fail(DiagnosisLayerAddress, started, "this address is not reached over iroh", nil, "")
		return *diagnosis
	}
	if bad := irohAddressFailure(parsed, started); bad != nil {
		diagnosis.add(*bad)
		return *diagnosis
	}
	// Cannot fail: Parse validated the ID and Value is what it produced.
	peer, _ := parsed.IrohID()
	diagnosis.add(irohAddressStep(parsed, peer, started))
	diagnoseIroh(ctx, diagnosis, e, parsed, peer, opts)
	return *diagnosis
}

// unreadableAddressHint is what to do about an address [Parse] refused.
//
// Which advice is right depends on what was being written. Parse validates the
// peer ID inside a discobox:// or iroh:// address, so one wrong character in a
// 56-symbol ID lands here rather than anywhere further down — and telling
// somebody who mistyped an ID that an address may also be a unix socket sends
// them to check the one part they got right.
func unreadableAddressHint(raw string) string {
	lowered := strings.ToLower(strings.TrimSpace(raw))
	if strings.HasPrefix(lowered, SchemeDiscobox+"://") || strings.HasPrefix(lowered, "iroh://") {
		return "A peer ID is what `discobox admin peer id` prints on the machine it belongs to, " +
			"and what the server logs at startup. Dashes and case are ignored, so it can be pasted " +
			"however it arrived; a single wrong character is caught here rather than as a timeout later."
	}
	return "An address is discobox://<peer-id>, unix://<path>, npipe://<name>, or http://<host>:<port>. " +
		"Set it with --server or DISCOBOX_SERVER."
}

// irohAddressFailure is the address layer's answer when an iroh address names
// no peer to dial, and nil when it names one.
//
// It covers the listen form only. A malformed ID never reaches it: Parse
// refuses the address itself, which is what [unreadableAddressHint] answers.
//
// Both callers need it: the package-level [Diagnose] to answer before it looks
// for an identity, and [IrohEndpoint.Diagnose] to answer before it dials. One
// function so the two cannot describe the same broken address differently.
func irohAddressFailure(parsed Endpoint, started time.Time) *DiagnosisStep {
	if parsed.Value != "" {
		return nil
	}
	return &DiagnosisStep{
		Layer:   DiagnosisLayerAddress,
		Status:  DiagnosisFailed,
		Summary: "this address names no peer to dial",
		Hint: "discobox:// with no peer ID is the listen form, which a server uses. " +
			"A client needs the server's peer ID: discobox://<peer-id>.",
		DurationMS: millis(started),
	}
}

// irohAddressStep is the address layer's answer for an address that names a
// peer: which peer, and whether it carries its own way in.
func irohAddressStep(parsed Endpoint, peer IrohID, started time.Time) DiagnosisStep {
	var detail []string
	if len(parsed.IrohAddrs) > 0 {
		detail = details("dialing without discovery, at " + strings.Join(parsed.IrohAddrs, " "))
	}
	return DiagnosisStep{
		Layer:      DiagnosisLayerAddress,
		Status:     DiagnosisOK,
		Summary:    "peer " + peer.String(),
		Detail:     detail,
		DurationMS: millis(started),
	}
}

// transportDescription says what carries this endpoint, in a phrase rather
// than a scheme: the scheme is already printed beside it, and what a reader
// needs to know is whether the thing that can fail is a file on this machine
// or a network.
func transportDescription(parsed Endpoint) string {
	switch parsed.Scheme {
	case "iroh":
		return "iroh · peer-to-peer QUIC, dialed by peer ID"
	case "unix":
		return "a unix socket on this machine"
	case "npipe":
		return "a named pipe on this machine"
	case "http", "https":
		return parsed.Scheme + " · an ordinary network address"
	default:
		return parsed.Scheme
	}
}

// diagnoseDialable covers every endpoint that is not iroh: a socket, a pipe,
// or an address. There is one layer under HTTP for all three — either the
// listener is there or it is not — so the connection and the request are one
// step, and only the server's own answer is another.
func diagnoseDialable(ctx context.Context, diagnosis *Diagnosis, parsed Endpoint, opts DiagnoseOptions) {
	started := time.Now()
	baseURL, client, err := HTTPClient(parsed.Raw, nil)
	if err != nil {
		diagnosis.fail(DiagnosisLayerConnect, started, "no client for this endpoint", err, "")
		diagnosis.skip(DiagnosisLayerServer, "the endpoint was never dialed")
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, opts.RequestTimeout)
	defer cancel()
	status, err := probeHealthClient(requestCtx, baseURL, client)
	if err != nil {
		diagnosis.fail(DiagnosisLayerConnect, started, "nothing answered at this endpoint", err,
			dialableHint(parsed))
		diagnosis.skip(DiagnosisLayerServer, "the endpoint was never reached")
		return
	}
	diagnosis.ok(DiagnosisLayerConnect, started, "connected")
	reportHealth(diagnosis, time.Now(), status)
}

func dialableHint(parsed Endpoint) string {
	if parsed.AutoLaunchable() {
		return "No server is listening here. Any discobox command starts one for you; `discobox admin server logs` says why the last one stopped."
	}
	return "Check that a server is listening on this address and that nothing between here and it is blocking the port."
}

// diagnoseIroh walks the transport's own layers. Each one is reported before
// the next is attempted, so a report that stops halfway says exactly how far a
// connection gets.
func diagnoseIroh(ctx context.Context, diagnosis *Diagnosis, configured *IrohEndpoint, target Endpoint, peer IrohID, opts DiagnoseOptions) {
	started := time.Now()
	nativeVersion, err := iroh.IrohVersion()
	if err != nil {
		diagnosis.fail(DiagnosisLayerRuntime, started, "the iroh library did not load", err,
			"Every build ships this library and extracts it on first use. Set IROH_GO_CACHE_DIR if the default cache directory is read-only or mounted noexec.")
		diagnosis.skipRest(DiagnosisLayerIdentity, DiagnosisLayerBind, DiagnosisLayerRelay,
			DiagnosisLayerConnect, DiagnosisLayerStream, DiagnosisLayerAdmission, DiagnosisLayerServer, DiagnosisLayerRoute)
		return
	}
	var platformDetail, libraryDetail string
	if platform := iroh.LibraryPlatform(); platform != "" {
		platformDetail = "platform " + platform
	}
	if path, pathErr := iroh.LibraryPath(); pathErr == nil && path != "" {
		libraryDetail = "library " + path
	}
	diagnosis.ok(DiagnosisLayerRuntime, started, "iroh "+nativeVersion, platformDetail, libraryDetail)

	started = time.Now()
	local, err := configured.ID()
	if err != nil {
		diagnosis.fail(DiagnosisLayerIdentity, started, "this machine's identity is unreadable", err, "")
		diagnosis.skipRest(DiagnosisLayerBind, DiagnosisLayerRelay, DiagnosisLayerConnect,
			DiagnosisLayerStream, DiagnosisLayerAdmission, DiagnosisLayerServer, DiagnosisLayerRoute)
		return
	}
	// What an operator enrolls comes first, because this row is what they read
	// when a server says they are not enrolled. With a certificate the key that
	// dials is generated per process and differs on every run, so printing it
	// here as "this machine" would hand them a value to enroll that is wrong
	// before they finish typing it (ADR 0104).
	if cert := configured.cfg.Certificate; cert != nil {
		diagnosis.ok(DiagnosisLayerIdentity, started, "this machine is "+cert.Issuer.String(),
			"dialing as "+local.Short()+", a key this process generated and certified")
	} else {
		diagnosis.ok(DiagnosisLayerIdentity, started, "this machine is "+local.String())
	}

	started = time.Now()
	bound, err := configured.bind()
	if err != nil {
		diagnosis.fail(DiagnosisLayerBind, started, "no local socket", err,
			"iroh needs a UDP socket. Something else holding the port, or a sandbox with no network, is what stops this.")
		diagnosis.skipRest(DiagnosisLayerRelay, DiagnosisLayerConnect, DiagnosisLayerStream,
			DiagnosisLayerAdmission, DiagnosisLayerServer, DiagnosisLayerRoute)
		return
	}
	boundSummary := "bound"
	if sockets, socketsErr := bound.BoundSockets(); socketsErr == nil && len(sockets) > 0 {
		boundSummary = "bound " + joinAddrPorts(sockets)
	}
	diagnosis.ok(DiagnosisLayerBind, started, boundSummary, irohReachDescription(configured.cfg))

	relayReached := diagnoseIrohRelay(ctx, diagnosis, configured, opts)
	diagnoseIrohConnect(ctx, diagnosis, configured, bound, target, peer, local, relayReached, opts)
}

// diagnoseIrohRelay reports whether this machine can reach a relay, and does
// not stop the diagnosis when it cannot.
//
// A relay is how two peers find each other and how they talk when neither can
// be reached directly. It is not required: a peer on the same network, or one
// dialed with ?addr=, connects without ever using one. So this is a warning
// rather than a failure, and what it buys is the sentence the connect layer
// prints if the dial then fails.
func diagnoseIrohRelay(ctx context.Context, diagnosis *Diagnosis, configured *IrohEndpoint, opts DiagnoseOptions) bool {
	started := time.Now()
	if configured.cfg.DisableRelay {
		diagnosis.skip(DiagnosisLayerRelay, "relays are turned off for this endpoint")
		return false
	}
	relayCtx, cancel := context.WithTimeout(ctx, opts.RelayTimeout)
	defer cancel()
	home, err := configured.Relay(relayCtx)
	if err != nil {
		diagnosis.add(DiagnosisStep{
			Layer:      DiagnosisLayerRelay,
			Status:     DiagnosisWarn,
			Summary:    fmt.Sprintf("no relay after %s", opts.RelayTimeout),
			Error:      errorText(err),
			Hint:       relayHint(configured.cfg),
			DurationMS: millis(started),
		})
		return false
	}
	if strings.TrimSpace(home) == "" {
		diagnosis.ok(DiagnosisLayerRelay, started, "this machine is online, with no home relay")
		return true
	}
	diagnosis.ok(DiagnosisLayerRelay, started, "this machine is on "+home)
	return true
}

func relayHint(cfg IrohConfig) string {
	if len(cfg.RelayURLs) > 0 {
		return "This machine cannot reach the relays it was given (" + strings.Join(cfg.RelayURLs, " ") +
			"). Both ends need the same list: the server's iroh.relayUrls and the client's --iroh-relay."
	}
	return "This machine cannot reach a public relay. Outbound HTTPS to relay.iroh.network is what a relay needs; a peer on this network, or one dialed with ?addr=<ip:port>, still works without one."
}

// diagnoseIrohConnect dials the peer and takes the exchange apart: the
// handshake, the stream, whether the server admitted us, and what it says
// about itself.
func diagnoseIrohConnect(ctx context.Context, diagnosis *Diagnosis, configured *IrohEndpoint, bound *iroh.Endpoint, target Endpoint, peer, local IrohID, relayReached bool, opts DiagnoseOptions) {
	started := time.Now()
	direct := append(append([]string(nil), target.IrohAddrs...), locate(configured.cfg.Locate, peer)...)
	parsedAddrs, err := parseIrohAddrs(direct)
	if err != nil {
		diagnosis.fail(DiagnosisLayerConnect, started, "an ?addr= parameter on this address is unusable", err,
			"Each ?addr= is an ip:port, as the server prints them in its \"without discovery, dial …\" line.")
		diagnosis.skipRest(DiagnosisLayerStream, DiagnosisLayerAdmission, DiagnosisLayerServer, DiagnosisLayerRoute)
		return
	}
	addr := iroh.AddrOf(iroh.EndpointID(peer)).WithDirectAddrs(parsedAddrs...)

	// Worked out before the dial rather than after it, because a dial that
	// fails is the one whose reader needs it. A connect layer that reports what
	// it was working with only when it succeeded tells you what you dialed
	// exactly when you no longer care.
	alpn := configured.dialALPN()
	foundBy := "dialed by peer ID alone, leaving discovery to find it"
	if len(parsedAddrs) > 0 {
		foundBy = "direct addresses tried: " + joinAddrPorts(parsedAddrs)
	}

	connectCtx, cancel := context.WithTimeout(ctx, opts.ConnectTimeout)
	defer cancel()
	conn, err := bound.Connect(connectCtx, addr, []byte(alpn))
	if err != nil {
		detail := append([]string{"alpn " + irohALPN, foundBy}, resolvedDetail(ctx, bound, peer)...)
		diagnosis.fail(DiagnosisLayerConnect, started, "the peer could not be reached", err,
			connectHint(len(parsedAddrs) > 0, relayReached), detail...)
		diagnosis.skipRest(DiagnosisLayerStream, DiagnosisLayerAdmission, DiagnosisLayerServer, DiagnosisLayerRoute)
		return
	}
	defer func() { _ = conn.CloseWithError(0, "") }()
	// The certificate this client owes a server it dialed on the certificate
	// ALPN. A diagnosis that skipped it would be refused for not presenting one
	// — and would report that as the server's doing rather than its own.
	if err := configured.presentCertificate(ctx, conn); err != nil {
		diagnosis.fail(DiagnosisLayerConnect, started, "this client's certificate could not be sent", err,
			"", "alpn "+alpn, foundBy)
		diagnosis.skipRest(DiagnosisLayerStream, DiagnosisLayerAdmission, DiagnosisLayerServer, DiagnosisLayerRoute)
		return
	}
	diagnosis.ok(DiagnosisLayerConnect, started, "handshake with "+peer.Short(), "alpn "+alpn, foundBy)

	started = time.Now()
	stream, err := conn.OpenConn(ctx)
	if err != nil {
		// A refusal can land here rather than on the request below. The server
		// closes an accepted connection the moment it decides against the peer
		// (ADR 0095 §4), and whether that close arrives before or after this
		// call is a race nothing on this side controls — so both places ask the
		// connection why it went away, and a refused peer is reported as one
		// either way. Blaming the stream sends an operator to look for a broken
		// transport when the answer is that they are not enrolled.
		if reason := irohCloseReason(conn); reason != "" {
			diagnosis.skip(DiagnosisLayerStream, "the connection was closed first")
			diagnosis.fail(DiagnosisLayerAdmission, started, "the server closed the connection: "+reason, nil,
				admissionHint(reason, enrolledIdentity(configured, local)))
			diagnosis.skip(DiagnosisLayerServer, "the connection was closed before the server answered")
			diagnosis.skip(DiagnosisLayerRoute, "the server closed the connection, and its paths went with it")
			return
		}
		diagnosis.fail(DiagnosisLayerStream, started, "no stream on the connection", err, "")
		diagnosis.skipRest(DiagnosisLayerAdmission, DiagnosisLayerServer, DiagnosisLayerRoute)
		return
	}
	defer func() { _ = stream.Close() }()
	diagnosis.ok(DiagnosisLayerStream, started, "opened")

	// The refusal lands here rather than at the handshake: a server accepts a
	// connection, checks the peer against its allowlist, and closes with the
	// reason if it says no (ADR 0095 §4). Opening a stream costs no round trip,
	// so this request is usually the first thing that can notice — usually,
	// because a close that arrives sooner fails the open above instead, which
	// is why that path asks the same question.
	started = time.Now()
	status, err := probeHealthConn(ctx, stream, opts.RequestTimeout)
	if err != nil {
		if reason := irohCloseReason(conn); reason != "" {
			diagnosis.fail(DiagnosisLayerAdmission, started, "the server closed the connection: "+reason, nil,
				admissionHint(reason, enrolledIdentity(configured, local)))
			diagnosis.skip(DiagnosisLayerServer, "the connection was closed before the server answered")
			diagnosis.skip(DiagnosisLayerRoute, "the server closed the connection, and its paths went with it")
			return
		}
		diagnosis.add(DiagnosisStep{
			Layer:   DiagnosisLayerAdmission,
			Status:  DiagnosisUnknown,
			Summary: "the server neither answered nor closed the connection",
		})
		diagnosis.fail(DiagnosisLayerServer, started, "no answer to "+health.Path, err, "")
		// The connection is still open here — it is the server that said
		// nothing — so the route is both askable and worth asking: how this
		// client reached a peer that will not answer is the next thing to
		// look at.
		diagnoseIrohRoute(diagnosis, conn)
		return
	}
	diagnosis.ok(DiagnosisLayerAdmission, started, "this peer is admitted")
	reportHealth(diagnosis, started, status)
	diagnoseIrohRoute(diagnosis, conn)
}

// diagnoseIrohRoute reports the path the connection settled on: direct to the
// peer's socket, or through a relay, and at what address.
//
// This is the question an operator asks first and the one the layers above
// cannot answer. "Connected" is the same word whether the packets are going
// straight to the machine or through a relay on another continent, and the two
// have completely different latency, completely different failure modes, and
// completely different things to check when something is slow.
//
// A connection usually has more than one path open: iroh races the direct
// addresses it knows against the relay and moves application data onto the
// best one that answers. The selected path is what traffic is on, and the rest
// are reported beside it, because a direct path sitting open and unselected is
// a different situation from no direct path at all.
//
// It carries no duration: reading the paths is a local call into the
// transport, so timing it would report how long this process took to ask
// itself a question.
func diagnoseIrohRoute(diagnosis *Diagnosis, conn *iroh.Conn) {
	paths, err := conn.Paths()
	if err != nil {
		diagnosis.add(DiagnosisStep{
			Layer:   DiagnosisLayerRoute,
			Status:  DiagnosisUnknown,
			Summary: "the transport did not report its paths",
			Error:   errorText(err),
		})
		return
	}
	selected, ok := selectedPath(paths)
	if !ok {
		// Not a failure of anything. The connection carried a request attempt
		// to get here, so it had a path: a snapshot that catches none is one
		// taken between two of them, or one taken as the last of them went
		// away. Reporting no route rather than guessing at one is the honest
		// answer to either.
		diagnosis.add(DiagnosisStep{
			Layer:   DiagnosisLayerRoute,
			Status:  DiagnosisUnknown,
			Summary: "the connection reports no path carrying traffic",
		})
		return
	}
	diagnosis.ok(DiagnosisLayerRoute, time.Time{}, routeSummary(selected), routeDetail(selected, paths)...)
}

func selectedPath(paths []iroh.Path) (iroh.Path, bool) {
	for _, path := range paths {
		if path.Selected {
			return path, true
		}
	}
	return iroh.Path{}, false
}

// routeSummary names the route in the two words an operator is looking for,
// then the address that backs them up.
func routeSummary(path iroh.Path) string {
	switch path.Kind {
	case iroh.PathIP:
		return "direct · " + path.Remote
	case iroh.PathRelay:
		return "relayed · " + path.Remote
	default:
		return string(path.Kind) + " · " + path.Remote
	}
}

// routeDetail is what the summary leaves out: how far away the peer is, and
// what else this connection could be using.
func routeDetail(selected iroh.Path, paths []iroh.Path) []string {
	others := make([]string, 0, len(paths))
	for _, path := range paths {
		if path.Selected {
			continue
		}
		others = append(others, string(path.Kind)+" "+path.Remote)
	}
	var alsoOpen string
	if len(others) > 0 {
		alsoOpen = "also open: " + strings.Join(others, ", ")
	}
	return details(routeRTT(selected.RTT), alsoOpen)
}

// routeRTT prints the round trip at the scale it happens to be on. A relay on
// another continent and a peer on this machine are three orders of magnitude
// apart, and rounding both to milliseconds reports the second one as zero.
func routeRTT(rtt time.Duration) string {
	switch {
	case rtt <= 0:
		return ""
	case rtt < time.Millisecond:
		return "rtt " + rtt.Round(time.Microsecond).String()
	default:
		return "rtt " + rtt.Round(time.Millisecond).String()
	}
}

// resolvedDetail says what the peer ID resolved to, for a dial that failed.
//
// It is the other half of "the peer could not be reached". Discovery returning
// nothing and discovery returning addresses that no longer answer are
// different failures with different fixes — a server that is not publishing
// against one that has moved — and the dial's own error is the same sentence
// either way.
//
// The addresses are read after the dial rather than before it because that is
// when they exist: a dial is what asks discovery, and what it learned outlives
// the attempt that failed. iroh warns they may be outdated or unusable, which
// is exactly the finding: every one inactive means these were had and none of
// them worked.
//
// A failure to read them is reported as not knowing rather than as a second
// error. This runs underneath a layer that has already failed, and the reader
// is looking at that failure; a second one about introspection would bury it.
func resolvedDetail(ctx context.Context, bound *iroh.Endpoint, peer IrohID) []string {
	resolveCtx, cancel := context.WithTimeout(ctx, diagnoseResolveWait)
	defer cancel()
	addrs, err := bound.RemoteAddrs(resolveCtx, iroh.EndpointID(peer))
	if err != nil {
		return details("what this ID resolved to could not be read: " + err.Error())
	}
	if len(addrs) == 0 {
		return details("this ID resolved to nothing: discovery has no addresses for this peer")
	}
	lines := []string{fmt.Sprintf("this ID resolved to %d %s, none of which answered:",
		len(addrs), pluralizeAddress(len(addrs)))}
	for _, addr := range addrs {
		lines = append(lines, "  "+string(addr.Kind)+" "+addr.Addr+" ("+string(addr.Usage)+")")
	}
	return lines
}

func pluralizeAddress(n int) string {
	if n == 1 {
		return "address"
	}
	return "addresses"
}

func connectHint(hasDirect, relayReached bool) string {
	switch {
	case hasDirect:
		return "The addresses on this endpoint did not answer. They are the server's own sockets, so they only work from a machine that can route to them; drop them to let discovery find the server instead."
	case !relayReached:
		return "With no relay reached and no ?addr= given, there is no way to find this peer. Fix the relay layer above, or dial the server's \"without discovery\" address, which it logs at startup."
	default:
		return "The peer ID resolved to nothing that answered. The relay row above is this machine's own relay and says nothing about whether the server reached one, so a green relay and a failed connect together mean the far end is missing rather than this one. Check that the server is running and listening on iroh (DISCOBOX_SERVER_LISTEN), and that its peer ID is the one in this address."
	}
}

// admissionHint turns the server's own close reason into the thing to do about
// it. The reason is written by the server's admission gate, for exactly this
// reader, so the wording is matched rather than parsed: an unrecognized reason
// is still printed, and only the advice is withheld.
// enrolledIdentity is the identity an operator would enroll for this client:
// the certificate's issuer where there is one, and otherwise the endpoint
// itself. A hint that named the per-process transport key would tell somebody
// to enroll a value that stops existing when the command does (ADR 0104).
func enrolledIdentity(configured *IrohEndpoint, local IrohID) IrohID {
	if cert := configured.cfg.Certificate; cert != nil {
		return cert.Issuer
	}
	return local
}

func admissionHint(reason string, local IrohID) string {
	lowered := strings.ToLower(reason)
	switch {
	case strings.Contains(lowered, "not authorized"), strings.Contains(lowered, "not enrolled"):
		return "This machine is not enrolled on that server. On a machine that already reaches it, run `discobox admin peer add " +
			local.String() + "`, or add that ID to <data dir>/authorized_ids on the server itself."
	case strings.Contains(lowered, "not finished starting"):
		return "The server is still starting; it admits peers once its database is open. Try again in a moment."
	case strings.Contains(lowered, "shutting down"):
		return "The server is shutting down."
	case strings.Contains(lowered, "allowlist"), strings.Contains(lowered, "enrollments"):
		return "The server could not read who is allowed in. Its own log says which file or query failed."
	default:
		return ""
	}
}

// reportHealth turns the server's readiness into the last layer's answer. A
// server that is still starting is not a failure of anything the transport
// did, so it is reported as a warning with the phase it is on.
func reportHealth(diagnosis *Diagnosis, started time.Time, status health.Status) {
	diagnosis.ServerStatus = status.Status
	summary := status.Status
	if summary == "" {
		summary = "answered"
	}
	if status.Starting() {
		diagnosis.add(DiagnosisStep{
			Layer:      DiagnosisLayerServer,
			Status:     DiagnosisWarn,
			Summary:    summary,
			Detail:     healthDetail(status),
			Hint:       "The server has bound its listeners and is still initializing. It answers everything with this until it is ready.",
			DurationMS: millis(started),
		})
		return
	}
	diagnosis.ok(DiagnosisLayerServer, started, summary, healthDetail(status)...)
}

func healthDetail(status health.Status) []string {
	var uptime string
	if status.UptimeSeconds > 0 {
		uptime = "up " + (time.Duration(status.UptimeSeconds) * time.Second).String()
	}
	return details(
		phaseDetail(status.Phase),
		versionDetail(status.Version),
		uptime,
	)
}

func phaseDetail(phase string) string {
	if phase = strings.TrimSpace(phase); phase == "" {
		return ""
	}
	return "phase " + phase
}

func versionDetail(value string) string {
	if value = strings.TrimSpace(value); value == "" {
		return ""
	}
	return "version " + value
}

// irohCloseReason is why the peer closed this connection, or "" if it is still
// open or ended without saying.
//
// It is the only place a refusal explains itself: an admission policy's error
// becomes the QUIC close reason, which is delivered to this side and nowhere
// else. Reading it costs a wait, so it is asked only once something has
// already failed.
func irohCloseReason(conn *iroh.Conn) string {
	ctx, cancel := context.WithTimeout(context.Background(), diagnoseCloseWait)
	defer cancel()
	// Wait always reports something, because a connection closed cleanly by
	// either side is still a close and that is the only way to tell it from the
	// many ways one can fail. What separates "closed" from "still open" here is
	// therefore our own deadline rather than a nil error.
	err := conn.Wait(ctx)
	if ctx.Err() != nil || errors.Is(err, iroh.ErrTimeout) || errors.Is(err, iroh.ErrCancelled) {
		return ""
	}
	var typed *iroh.Error
	if errors.As(err, &typed) {
		return strings.TrimSpace(typed.Msg)
	}
	return strings.TrimSpace(errorText(err))
}

// probeHealthConn asks /healthz over one already-open stream.
//
// It writes the request itself rather than building an *http.Client around the
// stream, because a client owns a connection pool and this is a diagnosis of
// one specific connection: the point is to learn what *this* stream does, and
// a transport that transparently redialed would hide the answer.
func probeHealthConn(ctx context.Context, conn net.Conn, timeout time.Duration) (health.Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LogicalHTTPBaseURL+health.Path, nil)
	if err != nil {
		return health.Status{}, err
	}
	req.Header.Set("User-Agent", diagnoseUserAgent)
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return health.Status{}, err
	}
	defer func() { _ = conn.SetDeadline(time.Time{}) }()
	if err := req.Write(conn); err != nil {
		return health.Status{}, fmt.Errorf("send the health request: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return health.Status{}, fmt.Errorf("read the health response: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeHealth(resp)
}

// probeHealthClient asks /healthz through an ordinary client, for the schemes
// whose transport has no layers of its own to take apart.
func probeHealthClient(ctx context.Context, baseURL string, client *http.Client) (health.Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+health.Path, nil)
	if err != nil {
		return health.Status{}, err
	}
	req.Header.Set("User-Agent", diagnoseUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return health.Status{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeHealth(resp)
}

// decodeHealth reads the readiness document. Both 200 and 503 carry one — 503
// is the server saying it is still starting — so the status code decides
// nothing here; the body does.
func decodeHealth(resp *http.Response) (health.Status, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return health.Status{}, fmt.Errorf("read the health response: %w", err)
	}
	var status health.Status
	if err := json.Unmarshal(body, &status); err != nil || status.Status == "" {
		return health.Status{}, fmt.Errorf("%s answered %s, which is not a Discobox server: %s",
			health.Path, resp.Status, firstLine(body))
	}
	return status, nil
}

func firstLine(body []byte) string {
	text := strings.TrimSpace(string(body))
	if text == "" {
		return "(empty response)"
	}
	if index := strings.IndexAny(text, "\r\n"); index >= 0 {
		text = text[:index]
	}
	const limit = 200
	if len(text) > limit {
		text = text[:limit] + "…"
	}
	return text
}

// add records one layer's answer. Steps are built whole and then added rather
// than added and then amended: a step is what one layer concluded, and the
// slice it lands in is not a place to keep editing.
func (d *Diagnosis) add(step DiagnosisStep) {
	d.Steps = append(d.Steps, step)
}

func (d *Diagnosis) ok(layer string, started time.Time, summary string, detail ...string) {
	d.add(DiagnosisStep{
		Layer:      layer,
		Status:     DiagnosisOK,
		Summary:    summary,
		Detail:     details(detail...),
		DurationMS: millis(started),
	})
}

// fail records a layer that stopped the ones above it. It takes the same
// trailing detail as [Diagnosis.ok] because a failure is where those facts are
// worth the most: what a layer was working with is how a reader tells one cause
// from another, and printing it only when the layer succeeded is backwards.
func (d *Diagnosis) fail(layer string, started time.Time, summary string, err error, hint string, detail ...string) {
	d.add(DiagnosisStep{
		Layer:      layer,
		Status:     DiagnosisFailed,
		Summary:    summary,
		Detail:     details(detail...),
		Error:      errorText(err),
		Hint:       hint,
		DurationMS: millis(started),
	})
}

func (d *Diagnosis) skip(layer, why string) {
	d.add(DiagnosisStep{Layer: layer, Status: DiagnosisSkipped, Summary: why})
}

// skipRest records the layers a failure stopped, so a report is the same shape
// whether or not it got to the end. A layer missing from a report and a layer
// that was never reached are different things, and only one of them is a bug.
func (d *Diagnosis) skipRest(layers ...string) {
	for _, layer := range layers {
		d.skip(layer, "not reached")
	}
}

// details drops the empty lines a caller assembled conditionally, so a step
// carries the facts that exist and no blanks where the others would have been.
func details(lines ...string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func millis(started time.Time) int64 {
	if started.IsZero() {
		return 0
	}
	return time.Since(started).Milliseconds()
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
