package discovm

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pool-agent/bundle"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

// poolKeyTTL is how long a key issued for a pool lasts. A new one is placed
// by half its life (renewAt), so a reconcile that cannot renew it for days
// leaves the pool working on the old one.
const poolKeyTTL = 7 * 24 * time.Hour

// poolKeys is how a pool's machine authenticates its engine (ADR
// 26-11-11-080 §9), in the first of these ways its driver offers: in-machine,
// where the driver gives the machine a credential of its own, and nothing is
// placed; a key the driver issues for the pool, which expires and cannot
// itself issue keys (ADR 26-10-23-551 §4); or else a sentinel shaped as the
// driver's key, which the pool's proxy swaps for the provider's key only where
// the driver declares it sends it (archil). The provider's own key is never
// placed on a pool's machine: a driver that offers none of these has its
// pools blocked.
type poolKeys struct {
	driver machine.Driver
	// inMachineField is the field of the driver's configuration that has
	// its machine authenticate as itself (inMachineField), or empty for a
	// driver that cannot: a pool's machine on one that can is given nothing.
	inMachineField string
	// sends are where the driver sends its key under the provider's
	// configuration, on a driver whose pools hold a sentinel for it: one
	// that neither authenticates in-machine nor issues keys, and declares
	// where it sends its one key (proxyagent.DriverKeySends, which the pool
	// agent holds its sentinel to as well). Empty is no sentinel.
	sends []machine.SecretSend
	// providerKey is the provider's own key, what a pool's proxy is handed
	// in a sentinel's place (resolve); never placed on a machine.
	providerKey string
	ttl         time.Duration
	now         func() time.Time
}

func newPoolKeys(driver machine.Driver, cfg settings) poolKeys {
	field, _ := inMachineField(driver.Name())
	k := poolKeys{
		driver:         driver,
		inMachineField: field,
		providerKey:    cfg.key,
		ttl:            poolKeyTTL,
		now:            time.Now,
	}
	if !k.inMachine() && driver.Capabilities().Keys.Mode == machine.KeysNone {
		// A driver that declares no place a proxy could swap its key in
		// has its pools blocked, as before (keyable).
		k.sends, _ = proxyagent.DriverKeySends(driver.Name(), cfg.driverConfig)
	}
	return k
}

// inMachine reports whether the driver's pools authenticate in-machine.
func (k poolKeys) inMachine() bool { return k.inMachineField != "" }

// sentinel reports whether the driver's pools hold a sentinel for its key.
func (k poolKeys) sentinel() bool { return len(k.sends) > 0 }

// keyable refuses, before any machine is made for it, a pool whose machine
// no key could ever reach: the driver authenticates no machine in-machine,
// issues no keys at all (machine.KeysNone), and declares no place a sentinel
// could be swapped for its key. Such a machine would be made and billed, then
// blocked at its first key (ensure), and started again at every pass. A
// driver that issues keys but refuses one is found by ensure.
func (k poolKeys) keyable(pool *model.Pool) error {
	if k.inMachine() || k.sentinel() || k.driver.Capabilities().Keys.Mode != machine.KeysNone {
		return nil
	}
	return k.blocked(pool, machine.ErrNoKey)
}

// blocked is the error of a pool whose machine its driver gives no key: the
// provider's own key is never placed there (ADR 26-11-11-080 §9).
func (k poolKeys) blocked(pool *model.Pool, err error) error {
	return fmt.Errorf("pool %s is blocked: the %s driver neither authenticates its machines in-machine, nor can issue one a key (%w), nor declares where it sends its key for the pool's proxy to swap a sentinel for it, and the provider's own key is never placed on a pool's machine", pool.ID, k.driver.Name(), err)
}

// poolKeyRecord is how a pool's machine authenticates, as the provider's
// record keeps it: never a secret.
type poolKeyRecord struct {
	// Issued is a key the driver issued for the pool. Neither it nor
	// InMachine is the provider's own key, which an older server placed:
	// it is taken off the machine.
	Issued bool `json:"issued"`
	// InMachine is a machine that authenticates as itself, given nothing.
	InMachine bool `json:"inMachine,omitempty"`
	// Sentinel is the sentinel placed for the driver's key, which the
	// pool's proxy swaps for the provider's key (ADR 26-11-11-080 §9). It
	// is no secret: a stand-in, which the server resolves only for this
	// pool, and only where the driver sends its key.
	Sentinel string `json:"sentinel,omitempty"`
	// ID and Name are the driver's for an issued key.
	ID       string        `json:"id,omitempty"`
	Name     string        `json:"name,omitempty"`
	Scope    machine.Scope `json:"scope,omitempty"`
	IssuedAt time.Time     `json:"issuedAt,omitzero"`
	Expires  time.Time     `json:"expires,omitzero"`
	// Fingerprint names the provider's own key an older server placed.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// renewAt is when an issued key is replaced: half its life.
func (r poolKeyRecord) renewAt() time.Time {
	return r.IssuedAt.Add(r.Expires.Sub(r.IssuedAt) / 2)
}

// keyPrefix is how every key issued for the pool is named, so a provider that
// lost its record of one finds it by name to revoke it. A pool's scope is
// of fixed length, so no pool's prefix is another's.
func keyPrefix(pool *model.Pool) (string, error) {
	scope, err := machinePool(pool.ID)
	if err != nil {
		return "", err
	}
	return scope + "-", nil
}

// ensure gives the pool's machine what its engine authenticates with,
// through guest, and returns the record of it, beside any error: current
// when nothing changed, and nil when it holds nothing.
//
// On a driver that authenticates in-machine nothing is placed. A key an older
// server placed (the provider's own, or one issued) is left where it is, and
// its record with it, for the agent still running on it: retire takes it off
// once the in-machine placement has applied. Until then an issued key is
// still renewed, but a renewal that fails never holds the switch up. With no
// record nothing is known of what the machine holds (a new machine, or a
// record lost), so none is returned, and retire takes off whatever an older
// server may have left.
//
// Otherwise a key still in its first half-life is kept. One past it is
// replaced with a newly issued one, and the old is left to expire rather than
// revoked, so no call in flight meets a revoked key. A renewal that fails in
// any way — not issued, not issuable, not placed — leaves the pool on the key
// it holds while that still works, and is tried again on the next reconcile:
// a pool that works is never failed, and so repaired, over a key it does not
// need yet. A driver that cannot issue at all blocks the pool, saying why,
// and the provider's key an older server placed is taken off.
func (k poolKeys) ensure(ctx context.Context, pool *model.Pool, guest machine.Guest, current *poolKeyRecord) (*poolKeyRecord, error) {
	if k.inMachine() {
		if current == nil || current.InMachine || !current.Issued {
			return current, nil
		}
		renewed, err := k.ensureIssued(ctx, pool, guest, current)
		if err != nil {
			slog.WarnContext(ctx, "the issued key of a pool moving to in-machine auth was not renewed", "pool", pool.ID, "key", current.ID, "error", err)
			return current, nil
		}
		return renewed, nil
	}
	if k.sentinel() {
		return k.ensureSentinel(ctx, pool, guest, current)
	}
	return k.ensureIssued(ctx, pool, guest, current)
}

// ensureSentinel is ensure on a driver whose pools hold a sentinel for its
// key: one placed is kept, for the pool's life. With none recorded, the
// sentinel the machine's key file already holds is taken as the record (a
// record lost: the running agent keeps the sentinel it registered with its
// proxy); with none there, or the provider's own key an older server placed,
// a new one is placed over it.
func (k poolKeys) ensureSentinel(ctx context.Context, pool *model.Pool, guest machine.Guest, current *poolKeyRecord) (*poolKeyRecord, error) {
	if current != nil && current.Sentinel != "" {
		return current, nil
	}
	if current == nil {
		if held, ok := k.heldSentinel(ctx, guest); ok {
			return &poolKeyRecord{Sentinel: held}, nil
		}
	}
	sentinel, err := k.newSentinel()
	if err != nil {
		return current, err
	}
	data, err := json.Marshal(machine.Key{Driver: k.driver.Name(), Name: sentinelKeyName, Secret: sentinel}) //nolint:gosec // G117: a sentinel, no secret, in the key file the pool's engine reads
	if err != nil {
		return current, err
	}
	if err := poolruntime.PlaceBoxFiles(ctx, guest, poolruntime.BoxFile{Path: bundle.ProvisioningKeyFile, Data: data}); err != nil {
		return current, fmt.Errorf("place pool %s's key sentinel: %w", pool.ID, err)
	}
	return &poolKeyRecord{Sentinel: sentinel}, nil
}

// sentinelKeyName names the key file a sentinel is placed in: what tells
// one this server placed from any other key a file holds, a real one an
// older server or a person put there included.
const sentinelKeyName = "discobox-sentinel"

// heldSentinel is the sentinel the machine's key file holds: one this server
// placed, by its name and its exact shape (newSentinel's). Anything else —
// the provider's key an older server placed, a key a person put there — is
// no sentinel, and is replaced, never kept on the disk as one.
func (k poolKeys) heldSentinel(ctx context.Context, guest machine.Guest) (string, bool) {
	data, err := readKeyFile(ctx, guest)
	if err != nil {
		return "", false
	}
	var key machine.Key
	if json.Unmarshal(data, &key) != nil || key.Driver != k.driver.Name() || key.Name != sentinelKeyName || !k.sentinelShaped(key.Secret) {
		return "", false
	}
	return key.Secret, true
}

// sentinelShaped reports whether s is what newSentinel makes: the driver's
// placeholder prefix, or none, then sentinelRandom's characters.
func (k poolKeys) sentinelShaped(s string) bool {
	body := s
	if secret, ok := secretOf(k.driver.Name()); ok {
		if prefix, _, found := strings.Cut(secret.Placeholder, "..."); found && prefix != "" {
			body = strings.TrimPrefix(s, prefix)
		}
	}
	if len(body) != sentinelRandomLen || !k.sentinelFound(s) {
		return false
	}
	for _, r := range body {
		if (r < 'a' || r > 'z') && (r < '2' || r > '7') {
			return false
		}
	}
	return true
}

// sentinelRandomLen is the length of a sentinel's random characters: 20
// random bytes in lower-case base32, unpadded.
const sentinelRandomLen = 32

// readKeyFile is the machine's provisioning key file, as it stands.
func readKeyFile(ctx context.Context, guest machine.Guest) ([]byte, error) {
	archive, err := guest.CopyFrom(ctx, bundle.ProvisioningKeyFile)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	tr := tar.NewReader(archive)
	if _, err := tr.Next(); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(tr, 64<<10))
}

// newSentinel is a sentinel for the driver's key: shaped as one (its
// placeholder's prefix, archil's "key-", then random characters), where that
// shape survives the driver's own rewriting of the key at every place it
// sends it, so the proxy finds it there; or else the random characters
// alone.
func (k poolKeys) newSentinel() (string, error) {
	var raw [20]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	random := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:]))
	if secret, ok := secretOf(k.driver.Name()); ok {
		if prefix, _, found := strings.Cut(secret.Placeholder, "..."); found && prefix != "" {
			shaped := prefix + random
			if k.sentinelFound(shaped) {
				return shaped, nil
			}
		}
	}
	if !k.sentinelFound(random) {
		return "", fmt.Errorf("the %s driver rewrites its key where it sends it, so no sentinel survives for the pool's proxy to find", k.driver.Name())
	}
	return random, nil
}

// sentinelFound reports whether sentinel stands whole at every place the
// driver sends its key, as the driver puts it there.
func (k poolKeys) sentinelFound(sentinel string) bool {
	for _, send := range k.sends {
		if !strings.Contains(sentAs(send, sentinel), sentinel) {
			return false
		}
	}
	return true
}

// sentAs is a key as the driver puts it at a place it sends it: in a header,
// after its value prefix and less its trimmed prefix; in a JSON field, as it
// is.
func sentAs(send machine.SecretSend, key string) string {
	if send.Header == "" {
		return key
	}
	return send.ValuePrefix + strings.TrimPrefix(key, send.TrimPrefix)
}

// resolve is what a pool's proxy puts in the place of the sentinel its
// machine holds, at a place the driver sends its key to host (ADR
// 26-11-11-080 §9): the provider's key, as the driver would put it there in
// place of the sentinel, so the request the proxy sends is the one the driver
// would send with the key itself. It is answered only for the sentinel the
// pool's record holds, and for a host the driver sends its key to.
func (k poolKeys) resolve(record *poolKeyRecord, sentinel, host string) (string, error) {
	if !k.sentinel() {
		return "", fmt.Errorf("%w: the %s driver's pools hold no sentinel", sandbox.ErrDriverKeyRefused, k.driver.Name())
	}
	if record == nil || record.Sentinel == "" || subtle.ConstantTimeCompare([]byte(record.Sentinel), []byte(sentinel)) != 1 {
		return "", fmt.Errorf("%w: not the sentinel this pool's machine holds", sandbox.ErrDriverKeyRefused)
	}
	if k.providerKey == "" {
		return "", fmt.Errorf("the %s provider has %w", k.driver.Name(), errNoKey)
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	value, found := "", false
	for _, send := range k.sends {
		if !slices.ContainsFunc(send.Hosts, func(hostPort string) bool {
			h, _, err := net.SplitHostPort(hostPort)
			return err == nil && strings.EqualFold(h, host)
		}) {
			continue
		}
		placed, err := inPlaceOf(send, sentinel, k.providerKey)
		if err != nil {
			return "", err
		}
		if found && placed != value {
			return "", fmt.Errorf("the %s driver sends its key to %s in more than one form, and a sentinel has one value", k.driver.Name(), host)
		}
		value, found = placed, true
	}
	if !found {
		return "", fmt.Errorf("%w: the %s driver never sends its key to %s", sandbox.ErrDriverKeyRefused, k.driver.Name(), host)
	}
	return value, nil
}

// inPlaceOf is what replaces sentinel where send carries it so the result is
// key as the driver would send it there: the sentinel stands within the
// driver's form of it (sentAs), and what is around it there must be around
// the key's form too.
func inPlaceOf(send machine.SecretSend, sentinel, key string) (string, error) {
	withSentinel, withKey := sentAs(send, sentinel), sentAs(send, key)
	at := strings.Index(withSentinel, sentinel)
	if at < 0 {
		return "", errors.New("the sentinel does not stand whole where the driver sends its key")
	}
	before, after := withSentinel[:at], withSentinel[at+len(sentinel):]
	if !strings.HasPrefix(withKey, before) || !strings.HasSuffix(withKey, after) || len(withKey) < len(before)+len(after) {
		return "", errors.New("the provider's key does not take the form the driver gives its sentinel")
	}
	return withKey[len(before) : len(withKey)-len(after)], nil
}

// ensureIssued is ensure for a key the driver issues the pool.
func (k poolKeys) ensureIssued(ctx context.Context, pool *model.Pool, guest machine.Guest, current *poolKeyRecord) (*poolKeyRecord, error) {
	now := k.now()
	if current != nil && current.Issued && now.Before(current.renewAt()) {
		return current, nil
	}
	// keep answers a renewal that failed: the key held stays while it works.
	keep := func(err error) (*poolKeyRecord, error) {
		if current != nil && current.Issued && now.Before(current.Expires) {
			slog.WarnContext(ctx, "keeping the pool's provisioning key until it expires: its renewal failed", "pool", pool.ID, "key", current.ID, "expires", current.Expires, "error", err)
			return current, nil
		}
		return current, err
	}
	prefix, err := keyPrefix(pool)
	if err != nil {
		return nil, err
	}
	name := prefix + strconv.FormatInt(now.Unix(), 36)
	issued, err := k.driver.IssueKey(ctx, machine.IssueRequest{Name: name, TTL: k.ttl})
	switch {
	case err == nil:
	case errors.Is(err, machine.ErrCannotIssue) || errors.Is(err, machine.ErrNoKey):
		blocked := k.blocked(pool, err)
		if current != nil && !current.Issued {
			// The provider's key an older server placed leaves the
			// machine, whatever else follows.
			if rerr := removeProvisioningKey(ctx, guest); rerr != nil {
				return current, errors.Join(blocked, fmt.Errorf("remove the provider's key from pool %s's machine: %w", pool.ID, rerr))
			}
			return nil, blocked
		}
		return keep(blocked)
	default:
		return keep(fmt.Errorf("issue pool %s's provisioning key: %w", pool.ID, notReachable(err)))
	}
	record := poolKeyRecord{Issued: true, ID: issued.ID, Name: issued.Name, Scope: issued.Scope, IssuedAt: now, Expires: issued.Expires}
	data, err := json.Marshal(issued) //nolint:gosec // G117: the key file the pool's engine reads, placed root's alone
	if err != nil {
		return nil, err
	}
	if err := poolruntime.PlaceBoxFiles(ctx, guest, poolruntime.BoxFile{Path: bundle.ProvisioningKeyFile, Data: data}); err != nil {
		// A key that never reached the machine is nobody's: take it back.
		if rerr := k.driver.RevokeKey(context.WithoutCancel(ctx), issued); rerr != nil {
			slog.WarnContext(ctx, "an issued key that was not placed is left to expire", "pool", pool.ID, "key", issued.ID, "error", rerr)
		}
		return keep(fmt.Errorf("place pool %s's provisioning key: %w", pool.ID, err))
	}
	k.sweepExpired(ctx, pool, prefix)
	return &record, nil
}

// retire ends the key an older server placed on a pool whose machine now
// authenticates in-machine, and returns the record that says so: called once
// the placement that turns in-machine auth on has applied, so the agent that
// still read the key file ran on it until it was replaced (ADR 26-11-11-080
// §9). The file is taken off the machine, and every key issued for the pool
// is revoked, found by name, so one read from the machine's disk stops
// working now rather than when it expires. With no record, both are done all
// the same: a record lost says nothing of what an older server placed. A step
// that fails keeps the record as it was, and the next reconcile does it again.
func (k poolKeys) retire(ctx context.Context, pool *model.Pool, guest machine.Guest, current *poolKeyRecord) (*poolKeyRecord, error) {
	if !k.inMachine() || (current != nil && current.InMachine) {
		return current, nil
	}
	if err := removeProvisioningKey(ctx, guest); err != nil {
		return current, fmt.Errorf("take any key an older server placed off pool %s's machine: %w", pool.ID, err)
	}
	if err := k.revokeAll(ctx, pool, current); err != nil {
		return current, err
	}
	return &poolKeyRecord{InMachine: true}, nil
}

// removeProvisioningKey takes the provisioning key off a pool's machine.
func removeProvisioningKey(ctx context.Context, guest machine.Guest) error {
	var stderr strings.Builder
	code, err := machine.Run(ctx, guest, machine.ExecRequest{Argv: []string{"rm", "-f", bundle.ProvisioningKeyFile}}, nil, io.Discard, &stderr)
	if err == nil && code != 0 {
		err = fmt.Errorf("rm exited with status %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return err
}

// sweepExpired revokes the pool's keys that have expired, which work no more,
// so they do not pile up at the driver across renewals. It is best effort.
func (k poolKeys) sweepExpired(ctx context.Context, pool *model.Pool, prefix string) {
	keys, err := k.driver.IssuedKeys(ctx, "")
	if err != nil {
		slog.WarnContext(ctx, "the pool's expired keys were not listed", "pool", pool.ID, "error", err)
		return
	}
	now := k.now()
	for _, key := range keys {
		if strings.HasPrefix(key.Name, prefix) && !key.Expires.IsZero() && !now.Before(key.Expires) {
			if err := k.driver.RevokeKey(ctx, key); err != nil {
				slog.WarnContext(ctx, "the pool's expired key was not revoked", "pool", pool.ID, "key", key.ID, "error", err)
			}
		}
	}
}

// revokeAll revokes every key issued for the pool, found by name, when it is
// deleted or its machine replaced (ADR 26-10-23-551 §4). A driver that issues
// none has none to revoke; nor has a provider whose key the driver will not
// list keys for, unless the pool's record says one was issued, which is then
// an error to retry rather than a key left behind.
func (k poolKeys) revokeAll(ctx context.Context, pool *model.Pool, recorded *poolKeyRecord) error {
	if k.driver.Capabilities().Keys.Mode != machine.KeysIssue {
		return nil
	}
	prefix, err := keyPrefix(pool)
	if err != nil {
		return err
	}
	keys, err := k.driver.IssuedKeys(ctx, "")
	if err != nil {
		refused := errors.Is(err, machine.ErrUnauthorized) || errors.Is(err, machine.ErrCannotIssue) || errors.Is(err, machine.ErrNoKey)
		if refused && (recorded == nil || !recorded.Issued) {
			return nil
		}
		return fmt.Errorf("list pool %s's keys to revoke them: %w", pool.ID, notReachable(err))
	}
	var errs []error
	for _, key := range keys {
		if strings.HasPrefix(key.Name, prefix) {
			if err := k.driver.RevokeKey(ctx, key); err != nil {
				errs = append(errs, fmt.Errorf("revoke pool %s's key %s: %w", pool.ID, key.ID, notReachable(err)))
			}
		}
	}
	return errors.Join(errs...)
}

// hostCredential is what the pool shows of the key its machine holds (ADR
// 26-10-23-551 §4), and the driver's own token beside it: a machine that is
// not isolated keeps what isolation strips, and the pool's machine is not.
func (k poolKeys) hostCredential(record *poolKeyRecord) *model.PoolHostCredential {
	credential := &model.PoolHostCredential{MachineToken: k.driver.Capabilities().Bare == machine.BareStrip}
	if record != nil {
		credential.Key, credential.Scope, credential.KeyID = model.PoolHostKeyProvider, string(record.Scope), record.ID
		if record.InMachine {
			credential.Key = model.PoolHostKeyInMachine
		}
		if record.Sentinel != "" {
			credential.Key = model.PoolHostKeySentinel
		}
		if record.Issued {
			credential.Key = model.PoolHostKeyIssued
			if !record.Expires.IsZero() {
				expires := record.Expires
				credential.ExpiresAt = &expires
			}
		}
	}
	if *credential == (model.PoolHostCredential{}) {
		return nil
	}
	return credential
}
