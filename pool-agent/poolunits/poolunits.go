// Package poolunits keeps a pool's box running the agent and proxy its maker
// placed (ADR 26-11-26-536 §§2, 4): `discobox-pool-agent units apply`.
//
// systemd holds every socket the agent, the proxy and the builder's mediator
// serve, in socket units rendered here from the bootstrap: a process that
// stops leaves its sockets listening, and the connections that arrive queue
// for whichever process accepts next. The agent and its proxy run in one of
// two slots of their template units. When what they start with moves (their
// binary, their units, or the bootstrap, environment and outbound routing
// placed beside them), apply either restarts the running slot, as every pool
// did before, or, for a pool whose bootstrap asks for it, hands over: it
// starts the idle slot beside the running one, tells the old slot to drain,
// and waits for the new one to take over (pool-agent/handover).
//
// Apply is idempotent: what has not moved since it last ran moves nothing.
// The pool bundle's pool-agent feature runs it, so a new agent takes over
// within the apply that lays it, before the builder's units start; the pool's
// maker runs it again after every apply, which is where a moved placement,
// or a box whose bundle was applied before any bootstrap was placed, takes
// effect.
package poolunits

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/fsdurable"
	"github.com/discobox-ai/discobox/pool-agent/bundle"
	"github.com/discobox-ai/discobox/pool-agent/handover"
)

// Role is which process a socket is passed to.
type Role int

const (
	// RoleAgent sockets are passed to the agent in either slot.
	RoleAgent Role = iota
	// RoleProxy sockets are passed to the proxy in either slot.
	RoleProxy
	// RoleMediator sockets start, and are passed to, the builder's mediator.
	RoleMediator
)

// The names of a pool's sockets (Socket.Name): what each process looks its
// listeners up by.
const (
	// IngressSocket is the agent's own listener, where the server reaches it.
	IngressSocket = "ingress"
	// ControlPlaneSocket is where the agent and the proxy reach the control
	// plane over the channel the server opens, on a pool that has one.
	ControlPlaneSocket = "control-plane"
	// OriginsSocket and AssetsSocket are the agent's, which the proxy
	// forwards sandboxes' origin and bundle fetches to.
	OriginsSocket = "origins"
	AssetsSocket  = "assets"
	// ProxySocket, CredentialsSocket, DNSSocket and TunnelsSocket are the
	// proxy's sandbox-facing listeners, and ProxyControlSocket its loopback
	// control API.
	ProxySocket        = "proxy"
	CredentialsSocket  = "credentials"
	DNSSocket          = "dns"
	TunnelsSocket      = "tunnels"
	ProxyControlSocket = "proxy-control"
	// MediatorSocket and RegistryFrontSocket are the builder's mediator's.
	MediatorSocket      = "buildkit"
	RegistryFrontSocket = "registry-front"
)

// Socket is one listening socket systemd holds for a pool.
type Socket struct {
	// Name is its FileDescriptorName=, which the process looks it up by
	// (handover.Listeners), and names its unit.
	Name string
	// Listen is its ListenStream=: an address and port, or a socket's path.
	Listen string
	Role   Role
}

// Unit is the socket's unit.
func (s Socket) Unit() string { return "discobox-pool-" + s.Name + ".socket" }

// socketUnitPrefix is every rendered socket unit's, so one no longer rendered
// is found to remove.
const (
	socketUnitPrefix = "discobox-pool-"
	socketUnitSuffix = ".socket"
)

// The units a box provisioned before slots ran the agent and the proxy as.
// Apply stops and removes them once, which cuts what they served: they bound
// their sockets themselves.
const (
	legacyAgentUnit = "discobox-pool-agent.service"
	legacyProxyUnit = "discobox-proxy.service"
)

// Config is what Apply needs of the pool and the box.
type Config struct {
	// SystemdDir is where units are laid: /etc/systemd/system.
	SystemdDir string
	// StateFile records the slot running and what it started with.
	StateFile string
	// Sockets are the pool's sockets, from its bootstrap.
	Sockets []Socket
	// HandOver says the pool hands over to a new slot rather than restarting
	// the running one: what the provider asked for in its bootstrap.
	HandOver bool
	// Binary is the agent's binary, whose content moving cycles the agent,
	// the proxy and the mediator, which all run it.
	Binary string
	// Inputs are the files the agent and proxy read once, at start, that the
	// pool's maker places: their content moving cycles the slot.
	Inputs []string
	// Booted reports whether systemd runs the box: a bundle applied in an
	// image's build renders and starts nothing.
	Booted bool
	// Systemctl runs systemctl.
	Systemctl Systemctl
	// TakeOver bounds the wait for a new slot to take over once the old one
	// is told to drain; zero is takeOverTimeout.
	TakeOver time.Duration
	Logger   *slog.Logger
}

// takeOverTimeout is how long a handover waits for the new agent and proxy
// to say they are active once the old ones are told to drain: the old ones
// stop accepting at once, so it is the new ones' own start that it waits on.
const takeOverTimeout = 2 * time.Minute

// Systemctl runs one systemctl command and returns what it printed.
type Systemctl func(ctx context.Context, args ...string) (string, error)

// ExecSystemctl runs the box's systemctl.
func ExecSystemctl(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "systemctl", args...) //nolint:gosec // G204: systemctl with the pool's own unit names
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return string(out), fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return string(out), fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

// state is what Apply last left running.
type state struct {
	Slot        string      `json:"slot,omitempty"`
	Fingerprint fingerprint `json:"fingerprint"`
}

// fingerprint is what the running slot started with.
type fingerprint struct {
	Binary string `json:"binary,omitempty"`
	Units  string `json:"units,omitempty"`
	Inputs string `json:"inputs,omitempty"`
	// Sockets digests the slots' socket units: what they listen on.
	Sockets string `json:"sockets,omitempty"`
}

// Apply has the box run, in a slot, the agent and proxy of the bundle and
// placement it holds now.
func Apply(ctx context.Context, cfg Config) error {
	if !cfg.Booted {
		return nil
	}
	a := &applier{cfg: cfg, log: cfg.Logger}
	if a.log == nil {
		a.log = slog.Default()
	}
	unlock, err := a.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return a.apply(ctx)
}

// Start starts the slot the box runs, when it is not running: what a socket
// starts when traffic finds nothing serving it. While an apply runs it waits
// for it, starting nothing: that apply starts the slot once it has bound its
// sockets, and one started here meanwhile would serve the sockets it is
// about to replace.
func Start(ctx context.Context, cfg Config) error {
	if !cfg.Booted {
		return nil
	}
	a := &applier{cfg: cfg, log: cfg.Logger}
	if err := a.awaitSystemd(ctx); err != nil {
		return err
	}
	// An apply under way is waited out rather than left: the socket that
	// started this is not started again while it waits, so its traffic
	// cannot reach the socket's trigger limit, and the apply starts the
	// slot itself, which this then finds running.
	wait, cancel := context.WithTimeout(ctx, startWait)
	defer cancel()
	lock, err := handover.Acquire(wait, cfg.StateFile+".lock")
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	current, err := readState(cfg.StateFile)
	if err != nil {
		return err
	}
	slot := current.Slot
	if slot == "" {
		slot = bundle.Slots[0]
	}
	_, err = cfg.Systemctl(ctx, "start", "--no-block", bundle.AgentUnit(slot), bundle.ProxyUnit(slot))
	return err
}

// ErrSlotDraining is what an apply that would stop or move a slot still
// draining from the last handover refuses with: it moves nothing, and an
// apply once that slot has exited does.
var ErrSlotDraining = errors.New("a slot still drains from the last handover")

// startWait bounds how long Start waits out an apply.
const startWait = 15 * time.Minute

// unitsInstances names every running `units start` a socket began.
const unitsInstances = "discobox-pool-units@*.service"

// socketsToBind reports whether the slots' sockets are bound again: one
// moved in this render (changed) or since the last apply recorded them, or
// one is not listening.
func (a *applier) socketsToBind(ctx context.Context, changed []string, recorded, now fingerprint) (bool, error) {
	if len(a.byRole(changed, RoleAgent, RoleProxy)) > 0 || recorded.Sockets != now.Sockets {
		return true, nil
	}
	for _, unit := range a.ofRole(RoleAgent, RoleProxy) {
		running, err := a.active(ctx, unit)
		if err != nil || !running {
			return err == nil, err
		}
	}
	return false, nil
}

type applier struct {
	cfg Config
	log *slog.Logger
}

// lock waits for systemd to answer, and then holds the units' lock beside
// the state file, so one apply or start runs at a time.
func (a *applier) lock(ctx context.Context) (func(), error) {
	if err := a.awaitSystemd(ctx); err != nil {
		return nil, err
	}
	lock, err := handover.Acquire(ctx, a.cfg.StateFile+".lock")
	if err != nil {
		return nil, err
	}
	return func() { _ = lock.Release() }, nil
}

// systemdTimeout bounds the wait for systemd to answer on its bus.
const systemdTimeout = 5 * time.Minute

// awaitSystemd waits for systemd to answer on its bus. Its directory under
// /run appears before it does, so a systemctl run straight after a box's
// start can fail at once, or return while systemd is still starting
// (pool-agent/REVIEW.md, "A machine's placement").
func (a *applier) awaitSystemd(ctx context.Context) error {
	wait, cancel := context.WithTimeout(ctx, systemdTimeout)
	defer cancel()
	for {
		out, _ := a.cfg.Systemctl(wait, "is-system-running")
		if state := strings.TrimSpace(out); state != "" && state != "offline" {
			return nil
		}
		select {
		case <-wait.Done():
			return errors.New("systemd did not answer in the box")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (a *applier) apply(ctx context.Context) error {
	changed, stale, err := a.render()
	if err != nil {
		return err
	}
	legacy, err := exists(filepath.Join(a.cfg.SystemdDir, legacyAgentUnit))
	if err != nil {
		return err
	}
	if _, err := a.cfg.Systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	current, err := readState(a.cfg.StateFile)
	if err != nil {
		return err
	}
	if legacy {
		if err := a.retireLegacy(ctx); err != nil {
			return err
		}
		current = state{}
	}
	now, err := a.fingerprint()
	if err != nil {
		return err
	}
	// The slots' sockets are bound again when one moved, in this render or
	// since the last apply recorded them (one refused below), or one is not
	// listening.
	rebound, err := a.socketsToBind(ctx, changed, current.Fingerprint, now)
	if err != nil {
		return err
	}
	// Closing a socket the pool no longer has, or binding the slots'
	// sockets again, stops what a draining slot still serves on: neither
	// runs while one drains. Nothing is recorded, and a socket left to close
	// keeps its unit file, so the next apply after that slot has exited
	// does both.
	if len(stale) > 0 || rebound {
		slotUnits := append(bundle.AgentUnits(), proxyUnits()...)
		if draining, err := a.anyDraining(ctx, slotUnits...); err != nil || draining {
			if err != nil {
				return err
			}
			return fmt.Errorf("%w: the pool's sockets moved, and move once it has exited", ErrSlotDraining)
		}
	}
	if err := a.removeStale(ctx, stale); err != nil {
		return err
	}

	// The slots' sockets first, with the agent and proxy down for as little
	// as it takes; the mediator's after they serve again, since its stop
	// drains builds.
	// Every one of the slots' sockets, not just the ones that moved: a
	// socket left listening while the slots are down starts `units start`.
	var slotSockets []string
	if rebound {
		slotSockets = a.ofRole(RoleAgent, RoleProxy)
	}
	stopped, err := a.rebind(ctx, slotSockets, append(bundle.AgentUnits(), proxyUnits()...))
	if err != nil {
		return err
	}
	slot := current.Slot
	if slot == "" {
		slot = bundle.Slots[0]
	}
	running, err := a.active(ctx, bundle.AgentUnit(slot))
	if err != nil {
		return err
	}
	binaryMoved := current.Fingerprint.Binary != now.Binary
	handedOver := false
	// A handover that has not taken yet is reported once what the box runs
	// is recorded: next, while the old slot drains.
	var notTakenOver error
	switch {
	case stopped || !running:
		if _, err := a.cfg.Systemctl(ctx, "start", bundle.AgentUnit(slot), bundle.ProxyUnit(slot)); err != nil {
			return err
		}
	case current.Fingerprint == now:
		if a.cfg.HandOver {
			if err := a.finishHandOver(ctx, slot); err != nil {
				return err
			}
		}
	case a.cfg.HandOver && a.ingressHeld():
		next := other(slot)
		serving, err := a.handOver(ctx, slot, next, state{Slot: slot, Fingerprint: current.Fingerprint}, state{Slot: next, Fingerprint: now})
		if serving == "" {
			return err
		}
		slot, handedOver, notTakenOver = serving, serving == next, err
	default:
		// One transaction, as before slots: the proxy reads what the agent
		// writes before it says it is ready.
		if _, err := a.cfg.Systemctl(ctx, "restart", bundle.AgentUnit(slot), bundle.ProxyUnit(slot)); err != nil {
			return err
		}
	}
	if err := a.enableOnly(ctx, slot); err != nil {
		return err
	}
	if err := writeState(a.cfg.StateFile, state{Slot: slot, Fingerprint: now}); err != nil {
		return err
	}
	mediatorRebound, err := a.rebind(ctx, a.byRole(changed, RoleMediator), []string{bundle.MediatorUnit})
	if err != nil {
		return err
	}
	if binaryMoved && !mediatorRebound {
		if err := a.restartMediator(ctx, handedOver); err != nil {
			return err
		}
	}
	return notTakenOver
}

func proxyUnits() []string {
	units := make([]string, 0, len(bundle.Slots))
	for _, slot := range bundle.Slots {
		units = append(units, bundle.ProxyUnit(slot))
	}
	return units
}

// byRole is the socket units of changed whose sockets are passed to one of
// roles.
func (a *applier) byRole(changed []string, roles ...Role) []string {
	return slices.DeleteFunc(a.ofRole(roles...), func(unit string) bool { return !slices.Contains(changed, unit) })
}

// ofRole is every socket unit whose socket is passed to one of roles.
func (a *applier) ofRole(roles ...Role) []string {
	var units []string
	for _, socket := range a.cfg.Sockets {
		if slices.Contains(roles, socket.Role) {
			units = append(units, socket.Unit())
		}
	}
	return units
}

// render lays the socket units and the slots' drop-ins naming them, and
// answers which socket units it changed and which it removed.
func (a *applier) render() (changed, stale []string, err error) {
	if err := os.MkdirAll(a.cfg.SystemdDir, 0o755); err != nil {
		return nil, nil, err
	}
	wanted := map[string]bool{}
	var agentSockets, proxySockets []string
	for _, socket := range a.cfg.Sockets {
		unit := socket.Unit()
		wanted[unit] = true
		switch socket.Role {
		case RoleAgent:
			agentSockets = append(agentSockets, unit)
		case RoleProxy:
			proxySockets = append(proxySockets, unit)
		}
		moved, err := writeIfChanged(filepath.Join(a.cfg.SystemdDir, unit), socketUnit(socket))
		if err != nil {
			return nil, nil, err
		}
		if moved {
			changed = append(changed, unit)
		}
	}
	for template, sockets := range map[string][]string{bundle.AgentTemplate: agentSockets, bundle.ProxyTemplate: proxySockets} {
		dropIn := filepath.Join(a.cfg.SystemdDir, template+".d", "sockets.conf")
		if _, err := writeIfChanged(dropIn, socketsDropIn(sockets)); err != nil {
			return nil, nil, err
		}
	}
	entries, err := os.ReadDir(a.cfg.SystemdDir)
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, socketUnitPrefix) && strings.HasSuffix(name, socketUnitSuffix) && !wanted[name] {
			stale = append(stale, name)
		}
	}
	return changed, stale, nil
}

// socketUnit renders socket's unit. Traffic that finds no slot serving it
// starts the slot the box runs (Start), and the mediator's starts the
// mediator.
func socketUnit(socket Socket) string {
	service := bundle.UnitsInstance(socket.Name)
	if socket.Role == RoleMediator {
		service = bundle.MediatorUnit
	}
	var b strings.Builder
	b.WriteString("# Rendered by `discobox-pool-agent units apply` from the pool's bootstrap.\n")
	b.WriteString("[Unit]\nDescription=Discobox pool socket " + socket.Name + "\n\n")
	b.WriteString("[Socket]\nListenStream=" + socket.Listen + "\n")
	b.WriteString("FileDescriptorName=" + socket.Name + "\n")
	b.WriteString("Service=" + service + "\n")
	if strings.HasPrefix(socket.Listen, "/") {
		b.WriteString("SocketMode=0600\nDirectoryMode=0700\nRemoveOnStop=yes\n")
	} else {
		b.WriteString("ReuseAddress=yes\n")
	}
	b.WriteString("\n[Install]\nWantedBy=sockets.target\n")
	return b.String()
}

// socketsDropIn is a slot template's drop-in naming the sockets its every
// instance is passed.
func socketsDropIn(units []string) string {
	var b strings.Builder
	b.WriteString("# Rendered by `discobox-pool-agent units apply` from the pool's bootstrap.\n[Unit]\n")
	for _, unit := range units {
		b.WriteString("After=" + unit + "\n")
	}
	b.WriteString("\n[Service]\n")
	for _, unit := range units {
		b.WriteString("Sockets=" + unit + "\n")
	}
	return b.String()
}

// retireLegacy stops and removes the units of a box provisioned before slots.
func (a *applier) retireLegacy(ctx context.Context) error {
	a.log.Info("retiring the pool agent's units from before slots", "units", []string{legacyAgentUnit, legacyProxyUnit})
	if _, err := a.cfg.Systemctl(ctx, "stop", legacyProxyUnit, legacyAgentUnit); err != nil {
		return err
	}
	if _, err := a.cfg.Systemctl(ctx, "disable", legacyProxyUnit, legacyAgentUnit); err != nil {
		return err
	}
	for _, unit := range []string{legacyAgentUnit, legacyProxyUnit} {
		if err := os.Remove(filepath.Join(a.cfg.SystemdDir, unit)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	_, err := a.cfg.Systemctl(ctx, "daemon-reload")
	return err
}

// removeStale closes and removes the socket units no longer rendered. The
// processes they were passed to keep their descriptors until they stop.
func (a *applier) removeStale(ctx context.Context, stale []string) error {
	if len(stale) == 0 {
		return nil
	}
	a.log.Info("closing the pool's sockets it no longer has", "sockets", stale)
	if _, err := a.cfg.Systemctl(ctx, append([]string{"disable", "--now"}, stale...)...); err != nil {
		return err
	}
	for _, unit := range stale {
		if err := os.Remove(filepath.Join(a.cfg.SystemdDir, unit)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	_, err := a.cfg.Systemctl(ctx, "daemon-reload")
	return err
}

// rebind binds sockets, socket units whose content moved or is new, under
// a stop of the units that serve them, and answers whether it stopped them:
// a socket whose address moved cannot be handed over. The sockets stop
// first, so no traffic starts a unit on the old ones meanwhile; the units are
// started again by the caller, or here when they were running and are not
// the slots (the mediator).
func (a *applier) rebind(ctx context.Context, sockets, units []string) (bool, error) {
	if len(sockets) == 0 {
		return false, nil
	}
	var running []string
	for _, unit := range units {
		active, err := a.active(ctx, unit)
		if err != nil {
			return false, err
		}
		if active {
			running = append(running, unit)
		}
	}
	a.log.Info("binding the pool's sockets", "sockets", sockets, "stopping", running)
	if _, err := a.cfg.Systemctl(ctx, append([]string{"stop"}, sockets...)...); err != nil {
		return false, err
	}
	// A socket whose own service is active is refused a start: a `units
	// start` that traffic began waits on this apply, and would hold its
	// socket down. It has nothing to do that this apply does not.
	if _, err := a.cfg.Systemctl(ctx, "stop", unitsInstances); err != nil {
		return false, err
	}
	if len(running) > 0 {
		if _, err := a.cfg.Systemctl(ctx, append([]string{"stop"}, running...)...); err != nil {
			return false, err
		}
	}
	if _, err := a.cfg.Systemctl(ctx, append([]string{"enable", "--now"}, sockets...)...); err != nil {
		return false, err
	}
	if slices.Contains(units, bundle.MediatorUnit) && len(running) > 0 {
		if _, err := a.cfg.Systemctl(ctx, append([]string{"start"}, running...)...); err != nil {
			return false, err
		}
	}
	return true, nil
}

// handOver starts next beside slot, tells slot to drain, and waits for next
// to take over (ADR 26-11-26-536 §4). It answers the slot that serves once it
// returns, "" when nothing was changed, and an error when that is not next or
// next has not said it took over. No slot that drains is ever stopped or
// restarted here: what it carries ends on its own, within handover.DrainBound.
//   - next still draining from the last handover is left to finish, and
//     nothing is changed: the next apply hands over once it has exited;
//   - a next that does not start, or that starts while slot cannot be asked
//     or told to drain, is stopped before slot was told: slot serves as it
//     was, and prev stays recorded;
//   - next is recorded, with now, and enabled before slot is told, so what
//     the box runs is on record whatever happens after;
//   - a next that does not say it took over within the wait (TakeOver), while
//     no unit of slot has begun to drain, is stopped and slot serves on; once
//     one has, slot is left to finish its drain and next is left running,
//     which systemd restarts until it takes over.
func (a *applier) handOver(ctx context.Context, slot, next string, prev, now state) (string, error) {
	a.log.Info("handing the pool over to a new agent", "from", slot, "to", next)
	nextUnits := []string{bundle.AgentUnit(next), bundle.ProxyUnit(next)}
	for _, unit := range nextUnits {
		draining, err := a.draining(ctx, unit)
		if err != nil {
			return "", err
		}
		if draining {
			return "", fmt.Errorf("%w: %s still drains, and the pool hands over to slot %s once it has exited", ErrSlotDraining, unit, next)
		}
	}
	// What is left of next is no drain (above): stopped, so it starts anew.
	if _, err := a.cfg.Systemctl(ctx, "stop", bundle.ProxyUnit(next), bundle.AgentUnit(next)); err != nil {
		return "", err
	}
	stopNext := func(cause error) (string, error) {
		ctx := context.WithoutCancel(ctx)
		_, stopErr := a.cfg.Systemctl(ctx, "stop", bundle.ProxyUnit(next), bundle.AgentUnit(next))
		return "", errors.Join(cause, stopErr, a.record(ctx, prev))
	}
	// Both are Type=notify, and say they are ready as standbys: prepared,
	// accepting nothing until the old ones let go.
	if _, err := a.cfg.Systemctl(ctx, append([]string{"start"}, nextUnits...)...); err != nil {
		return stopNext(fmt.Errorf("start the pool's new agent in slot %s: %w", next, err))
	}
	if err := a.record(ctx, state{Slot: next, Fingerprint: now.Fingerprint}); err != nil {
		return stopNext(err)
	}
	// Each one apart: a proxy that is not running has nothing to drain, and
	// the new one takes over from no one. Until one is told, slot serves as
	// it was, and a failure only stops next.
	signaled := false
	for _, unit := range []string{bundle.AgentUnit(slot), bundle.ProxyUnit(slot)} {
		running, err := a.active(ctx, unit)
		if err != nil {
			if !signaled {
				return stopNext(err)
			}
			return next, err
		}
		if !running {
			continue
		}
		if err := a.tellToDrain(ctx, unit); err != nil {
			if !signaled {
				return stopNext(err)
			}
			// What is left untold is told by the next apply (finishHandOver).
			return next, err
		}
		signaled = true
	}
	if err := a.awaitTakeOver(ctx, nextUnits); err != nil {
		err = fmt.Errorf("the pool's new agent in slot %s did not take over: %w", next, err)
		draining, statusErr := a.anyDraining(ctx, bundle.AgentUnit(slot), bundle.ProxyUnit(slot))
		if statusErr == nil && !draining {
			return stopNext(err)
		}
		a.log.Warn("the pool's new agent has not taken over; the old one drains, and the new one is restarted until it does", "from", slot, "to", next, "error", err)
		return next, err
	}
	return next, nil
}

// drainSignalTries is how many times a slot's unit is told to drain before
// the handover gives up on it.
const drainSignalTries = 3

// tellToDrain sends unit the drain signal, trying again on a failure.
func (a *applier) tellToDrain(ctx context.Context, unit string) error {
	var err error
	for try := range drainSignalTries {
		if try > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(try) * 200 * time.Millisecond):
			}
		}
		if _, err = a.cfg.Systemctl(ctx, "kill", "--kill-who=main", "--signal=SIGUSR2", unit); err == nil {
			return nil
		}
	}
	return fmt.Errorf("tell %s to drain: %w", unit, err)
}

// finishHandOver ends what a handover left of the slot other than serving:
// a unit of it that still says it is active (a signal that reached only some
// of its units) is told to drain, and one that is a standby (an old process
// restarted mid-drain) is stopped, since it serves nothing and would only
// wait on the serving slot's lock. The unit taking over from one never told
// waits on its lock, and no apply moves anything while the fingerprint
// stands, so this is what ends it. A unit that drains is left to finish.
func (a *applier) finishHandOver(ctx context.Context, serving string) error {
	old := other(serving)
	for _, unit := range []string{bundle.AgentUnit(old), bundle.ProxyUnit(old)} {
		running, err := a.active(ctx, unit)
		if err != nil {
			return err
		}
		if !running {
			continue
		}
		status, err := a.status(ctx, unit)
		if err != nil {
			return err
		}
		switch status {
		case handover.StatusActive:
			a.log.Info("telling the pool's old slot to drain, as its handover left it", "unit", unit)
			if err := a.tellToDrain(ctx, unit); err != nil {
				return err
			}
		case handover.StatusStandby:
			a.log.Info("stopping a standby of the pool's old slot", "unit", unit)
			if _, err := a.cfg.Systemctl(ctx, "stop", unit); err != nil {
				return err
			}
		}
	}
	return nil
}

// draining reports whether unit runs and says it drains: a unit that has
// exited does not, whatever its last status said.
func (a *applier) draining(ctx context.Context, unit string) (bool, error) {
	running, err := a.active(ctx, unit)
	if err != nil || !running {
		return false, err
	}
	status, err := a.status(ctx, unit)
	return status == handover.StatusDraining, err
}

// record writes st as the state, and enables its slot alone.
func (a *applier) record(ctx context.Context, st state) error {
	if err := a.enableOnly(ctx, st.Slot); err != nil {
		return err
	}
	return writeState(a.cfg.StateFile, st)
}

// anyDraining reports whether any of units runs and says it drains.
func (a *applier) anyDraining(ctx context.Context, units ...string) (bool, error) {
	for _, unit := range units {
		draining, err := a.draining(ctx, unit)
		if err != nil || draining {
			return draining, err
		}
	}
	return false, nil
}

// status is what unit says it is (STATUS=), "" for one that says nothing.
func (a *applier) status(ctx context.Context, unit string) (string, error) {
	out, err := a.cfg.Systemctl(ctx, "show", "--property=StatusText", "--value", unit)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// awaitTakeOver waits, up to TakeOver, for every one of units to say it is
// active.
func (a *applier) awaitTakeOver(ctx context.Context, units []string) error {
	wait := a.cfg.TakeOver
	if wait <= 0 {
		wait = takeOverTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for _, unit := range units {
		if err := a.awaitStatus(waitCtx, unit, handover.StatusActive); err != nil {
			return err
		}
	}
	return nil
}

// awaitStatus waits until unit says it is in status.
func (a *applier) awaitStatus(ctx context.Context, unit, status string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	last := ""
	for {
		out, err := a.cfg.Systemctl(ctx, "show", "--property=StatusText", "--value", unit)
		if err == nil {
			last = strings.TrimSpace(out)
			if last == status {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s says %q: %w", unit, last, ctx.Err())
		case <-ticker.C:
		}
	}
}

// enableOnly has slot start at boot, and the other slot not.
func (a *applier) enableOnly(ctx context.Context, slot string) error {
	if _, err := a.cfg.Systemctl(ctx, "enable", bundle.AgentUnit(slot), bundle.ProxyUnit(slot)); err != nil {
		return err
	}
	o := other(slot)
	_, err := a.cfg.Systemctl(ctx, "disable", bundle.AgentUnit(o), bundle.ProxyUnit(o))
	return err
}

// restartMediator restarts the builder's mediator, which runs the agent's
// binary, once egress has returned. It drains the builds on it
// (buildkitagent.BuildDrain) while its socket queues new ones. After a
// handover nothing waits for that drain: the new agent is serving, and the
// pool's maker waits on this apply.
func (a *applier) restartMediator(ctx context.Context, handedOver bool) error {
	running, err := a.active(ctx, bundle.MediatorUnit)
	if err != nil || !running {
		return err
	}
	args := []string{"try-restart", bundle.MediatorUnit}
	if handedOver {
		args = []string{"try-restart", "--no-block", bundle.MediatorUnit}
	}
	_, err = a.cfg.Systemctl(ctx, args...)
	return err
}

// ingressHeld reports whether systemd holds the agent's own listener, which a
// handover needs: an agent that binds its own (a VSOCK one) cannot share it.
func (a *applier) ingressHeld() bool {
	for _, socket := range a.cfg.Sockets {
		if socket.Name == IngressSocket {
			return true
		}
	}
	return false
}

// active reports whether unit is active.
func (a *applier) active(ctx context.Context, unit string) (bool, error) {
	out, err := a.cfg.Systemctl(ctx, "show", "--property=ActiveState", "--value", unit)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "active", nil
}

// fingerprint is what the slot would start with now.
func (a *applier) fingerprint() (fingerprint, error) {
	binary, err := hashFiles([]string{a.cfg.Binary})
	if err != nil {
		return fingerprint{}, err
	}
	units, err := hashFiles([]string{
		filepath.Join(a.cfg.SystemdDir, bundle.AgentTemplate),
		filepath.Join(a.cfg.SystemdDir, bundle.ProxyTemplate),
		filepath.Join(a.cfg.SystemdDir, bundle.AgentTemplate+".d", "sockets.conf"),
		filepath.Join(a.cfg.SystemdDir, bundle.ProxyTemplate+".d", "sockets.conf"),
	})
	if err != nil {
		return fingerprint{}, err
	}
	inputs, err := hashFiles(a.cfg.Inputs)
	if err != nil {
		return fingerprint{}, err
	}
	var socketFiles []string
	for _, unit := range a.ofRole(RoleAgent, RoleProxy) {
		socketFiles = append(socketFiles, filepath.Join(a.cfg.SystemdDir, unit))
	}
	sockets, err := hashFiles(socketFiles)
	if err != nil {
		return fingerprint{}, err
	}
	return fingerprint{Binary: binary, Units: units, Inputs: inputs, Sockets: sockets}, nil
}

// hashFiles digests the named files' contents, a file not there included as
// not there.
func hashFiles(paths []string) (string, error) {
	sum := sha256.New()
	for _, path := range paths {
		_, _ = io.WriteString(sum, path+"\x00")
		file, err := os.Open(path) //nolint:gosec // G304: the pool's own files
		if errors.Is(err, os.ErrNotExist) {
			_, _ = io.WriteString(sum, "absent\x00")
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = io.Copy(sum, file)
		_ = file.Close()
		if err != nil {
			return "", err
		}
		_, _ = io.WriteString(sum, "\x00")
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func other(slot string) string {
	if slot == bundle.Slots[0] {
		return bundle.Slots[1]
	}
	return bundle.Slots[0]
}

func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// writeIfChanged writes content to path unless it holds it already, and
// answers whether it wrote.
func writeIfChanged(path, content string) (bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the pool's own units
	if err == nil && string(data) == content {
		return false, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, fsdurable.WriteFile(path, []byte(content), 0o644)
}

func readState(path string) (state, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the pool's own state
	if errors.Is(err, os.ErrNotExist) {
		return state{}, nil
	}
	if err != nil {
		return state{}, err
	}
	var current state
	if err := json.Unmarshal(data, &current); err != nil {
		// Read as nothing recorded: the next apply restarts what runs, once.
		return state{}, nil //nolint:nilerr // a record cut short is no record
	}
	if !slices.Contains(bundle.Slots, current.Slot) {
		current.Slot = ""
	}
	return current, nil
}

func writeState(path string, current state) error {
	data, err := json.Marshal(current)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsdurable.WriteFile(path, data, 0o600)
}
