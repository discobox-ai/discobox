package discovm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pool-agent/bundle"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

// poolKeyTTL is how long a key issued for a pool lasts. A new one is placed
// by half its life (renewAt), so a reconcile that cannot renew it for days
// leaves the pool working on the old one.
const poolKeyTTL = 7 * 24 * time.Hour

// poolKeys is how a pool's machine is given the provisioning key its engine
// calls the driver with (ADR 26-10-23-551 §4): one the driver issues for the
// pool, which expires and cannot itself issue keys; or, only where the
// provider's configuration opts in, the provider's own.
type poolKeys struct {
	driver machine.Driver
	// own is the provider's own key, which is placed only when deliver is
	// set and the driver cannot issue one.
	own     machine.Key
	deliver bool
	ttl     time.Duration
	now     func() time.Time
}

func newPoolKeys(driver machine.Driver, cfg Config) poolKeys {
	return poolKeys{
		driver:  driver,
		own:     machine.Key{Driver: driver.Name(), Secret: strings.TrimSpace(cfg.APIKey)},
		deliver: cfg.DeliverProviderKey,
		ttl:     poolKeyTTL,
		now:     time.Now,
	}
}

// check refuses a provider that opts in to delivering a key it does not have.
func (k poolKeys) check() error {
	if k.deliver && k.own.Secret == "" {
		return fmt.Errorf("the %s provider opts in to delivering its own key to its pools' machines, and has no API key", ProviderType)
	}
	return nil
}

// poolKeyRecord is the provisioning key placed on a pool's machine, as the
// provider's record keeps it: never its secret.
type poolKeyRecord struct {
	// Issued is a key the driver issued for the pool; false is the
	// provider's own.
	Issued bool `json:"issued"`
	// ID and Name are the driver's for an issued key.
	ID       string        `json:"id,omitempty"`
	Name     string        `json:"name,omitempty"`
	Scope    machine.Scope `json:"scope,omitempty"`
	IssuedAt time.Time     `json:"issuedAt,omitzero"`
	Expires  time.Time     `json:"expires,omitzero"`
	// Fingerprint names the provider's own key placed, so a key its
	// configuration rotates is placed again.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// renewAt is when an issued key is replaced: half its life.
func (r poolKeyRecord) renewAt() time.Time {
	return r.IssuedAt.Add(r.Expires.Sub(r.IssuedAt) / 2)
}

func fingerprint(key machine.Key) string {
	sum := sha256.Sum256([]byte(key.Driver + "\x00" + key.Secret))
	return hex.EncodeToString(sum[:8])
}

// keyPrefix is how every key issued for the pool is named, so a provider that
// lost its record of one finds it by name to revoke it. A pool's project is
// of fixed length, so no pool's prefix is another's.
func keyPrefix(pool *model.Pool) (string, error) {
	project, err := poolProject(pool.ID)
	if err != nil {
		return "", err
	}
	return project + "-", nil
}

// providerKeyRecheck is how often a pool holding the provider's own key asks
// the driver again for a narrower one, which a driver given a key that can
// issue (a boxd session's, in place of an API key) makes.
const providerKeyRecheck = 24 * time.Hour

// ensure gives the pool's machine a key that is not due for renewal, through
// guest, and returns the record of what the machine now holds, beside any
// error: current when nothing was placed, and nil when it holds none.
//
// A key still in its first half-life is kept. One past it is replaced with a
// newly issued one, and the old is left to expire rather than revoked, so no
// call in flight meets a revoked key. A renewal that fails in any way — not
// issued, not issuable, not placed — leaves the pool on the key it holds
// while that still works, and is tried again on the next reconcile: a pool
// that works is never failed, and so repaired, over a key it does not need
// yet. An issue that fails is retried, never answered with the provider's
// own key: that is placed only when the driver cannot issue one at all and
// the provider opts in, and is asked to be replaced by an issued one daily.
// Without the opt-in such a pool is blocked, saying why, and a provider's
// key placed before the opt-in was withdrawn is taken off the machine.
func (k poolKeys) ensure(ctx context.Context, pool *model.Pool, guest machine.Guest, current *poolKeyRecord) (*poolKeyRecord, error) {
	now := k.now()
	ownPlaced := current != nil && !current.Issued && k.deliver && current.Fingerprint == fingerprint(k.own)
	switch {
	case current != nil && current.Issued && now.Before(current.renewAt()):
		return current, nil
	case ownPlaced && now.Before(current.IssuedAt.Add(providerKeyRecheck)):
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
	var key machine.Key
	var record poolKeyRecord
	switch {
	case err == nil:
		key = issued
		record = poolKeyRecord{Issued: true, ID: issued.ID, Name: issued.Name, Scope: issued.Scope, IssuedAt: now, Expires: issued.Expires}
	case errors.Is(err, machine.ErrCannotIssue) || errors.Is(err, machine.ErrNoKey):
		if ownPlaced {
			// Still none to be had: the provider's key stays, asked about
			// again in a day.
			kept := *current
			kept.IssuedAt = now
			return &kept, nil
		}
		if !k.deliver {
			blocked := fmt.Errorf("pool %s is blocked: the %s driver cannot issue its machine a key (%w), and the provider does not opt in to delivering its own (deliverProviderKey)", pool.ID, k.driver.Name(), err)
			if current != nil && !current.Issued {
				// The opt-in was withdrawn: the provider's key leaves the
				// machine, whatever else follows.
				if rerr := removeProvisioningKey(ctx, guest); rerr != nil {
					return current, errors.Join(blocked, fmt.Errorf("remove the provider's key from pool %s's machine: %w", pool.ID, rerr))
				}
				return nil, blocked
			}
			return keep(blocked)
		}
		key = k.own
		record = poolKeyRecord{Issued: false, IssuedAt: now, Fingerprint: fingerprint(k.own)}
	default:
		return keep(fmt.Errorf("issue pool %s's provisioning key: %w", pool.ID, err))
	}
	data, err := json.Marshal(key) //nolint:gosec // G117: the key file the pool's engine reads, placed root's alone
	if err != nil {
		return nil, err
	}
	if err := poolruntime.PlaceBoxFiles(ctx, guest, poolruntime.BoxFile{Path: bundle.ProvisioningKeyFile, Data: data}); err != nil {
		if record.Issued {
			// A key that never reached the machine is nobody's: take it back.
			if rerr := k.driver.RevokeKey(context.WithoutCancel(ctx), issued); rerr != nil {
				slog.WarnContext(ctx, "an issued key that was not placed is left to expire", "pool", pool.ID, "key", issued.ID, "error", rerr)
			}
		}
		return keep(fmt.Errorf("place pool %s's provisioning key: %w", pool.ID, err))
	}
	if record.Issued {
		k.sweepExpired(ctx, pool, prefix)
	}
	return &record, nil
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
		return fmt.Errorf("list pool %s's keys to revoke them: %w", pool.ID, err)
	}
	var errs []error
	for _, key := range keys {
		if strings.HasPrefix(key.Name, prefix) {
			if err := k.driver.RevokeKey(ctx, key); err != nil {
				errs = append(errs, fmt.Errorf("revoke pool %s's key %s: %w", pool.ID, key.ID, err))
			}
		}
	}
	return errors.Join(errs...)
}

// hostCredential is what the pool shows of the key its machine holds (ADR
// 26-10-23-551 §4), and the driver's own token beside it: a machine that is
// not isolated keeps what isolation strips, and the pool's machine is not.
func (k poolKeys) hostCredential(record *poolKeyRecord) *model.PoolHostCredential {
	credential := &model.PoolHostCredential{MachineToken: k.driver.Capabilities().Isolation}
	if record != nil {
		credential.Key, credential.Scope, credential.KeyID = model.PoolHostKeyProvider, string(record.Scope), record.ID
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
