// Package rawsecrets keeps the raw values a sandbox's raw bindings hand it
// (ADR 26-10-27-840 §3): asked for from the pool over the agent's own mTLS
// link, kept one per file under sandboxconfig.RawSecretsDir on the sandbox's
// own /run tmpfs, and nowhere else.
//
// The sandbox is told, then pulls. The applied runtime-config document names
// the raw secrets it holds, never their values (RuntimeConfig.RawSecrets);
// the Holder asks the pool for the values when that set changes, and while it
// is not empty, and holds nothing — and asks for nothing — while it is.
//
// A process is given a raw secret by its variable: a file-delivered one's
// variable holds the file's path; an env-delivered one's holds the value,
// which the exec shim reads from the file into that process's environment as
// it starts (execs.StartRequest.RawEnv), so no record the agent keeps or
// reports ever carries it.
package rawsecrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

const (
	// PollInterval is how often the pool is asked: a binding made, changed or
	// removed reaches the sandbox within it.
	PollInterval = 10 * time.Second
	// expirySkew is how long before a value's expiry it is asked for again,
	// so a renewed token is in place before the old one lapses.
	expirySkew = 30 * time.Second
	// retryInterval is the pause after a failed ask.
	retryInterval = 5 * time.Second
	// AwaitBound is the longest AwaitFetched holds a launch for a set of
	// raw secrets the pool has not yet answered for. Past it the launch goes
	// on without them, and says so: a pool that cannot answer must not keep
	// the sandbox from being worked in.
	AwaitBound = 30 * time.Second
)

// Fetch asks the pool for every raw secret the sandbox holds now.
type Fetch func(ctx context.Context) (sandboxconfig.RawSecretsResponse, error)

// Owner is the account the files are owned by: the sandbox user. Nil fields
// leave them root's.
type Owner struct {
	UID *int64
	GID *int64
}

// Config configures a Holder. The zero value keeps files at
// sandboxconfig.RawSecretsDir, owned by this process, after checking that
// directory is on a memory filesystem.
type Config struct {
	Dir   string
	Owner Owner
	// OnMemory reports an error unless dir is on a filesystem kept in memory.
	// Nil checks it (onMemory); a test passes its own.
	OnMemory func(dir string) error
	Logger   *slog.Logger
	Now      func() time.Time
}

type held struct {
	delivery  string
	value     string
	expiresAt *time.Time
}

// Holder is the sandbox's raw secrets as last delivered.
type Holder struct {
	dir      string
	owner    Owner
	onMemory func(string) error
	logger   *slog.Logger
	now      func() time.Time

	mu   sync.RWMutex
	held map[string]held
	// applied is the set the applied document names, and told whether one
	// has been applied at all. gen counts the changes to it, from genAt;
	// fetchedGen is the last gen the pool answered for, and settled closes
	// when it reaches gen.
	applied    map[string]string
	told       bool
	gen        int64
	genAt      time.Time
	fetchedGen int64
	settled    chan struct{}
	wake       chan struct{}
}

// New returns an empty Holder.
func New(cfg Config) *Holder {
	h := &Holder{
		dir:      cfg.Dir,
		owner:    cfg.Owner,
		onMemory: cfg.OnMemory,
		logger:   cfg.Logger,
		now:      cfg.Now,
		held:     map[string]held{},
		settled:  make(chan struct{}),
		wake:     make(chan struct{}, 1),
	}
	if h.dir == "" {
		h.dir = sandboxconfig.RawSecretsDir
	}
	if h.onMemory == nil {
		h.onMemory = onMemory
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	if h.now == nil {
		h.now = time.Now
	}
	return h
}

// Dir is where the files are kept.
func (h *Holder) Dir() string { return h.dir }

// Path is the file a raw secret delivered in name is kept in.
func (h *Holder) Path(name string) string { return filepath.Join(h.dir, name) }

// Deliveries is every raw secret held now, variable to
// sandboxconfig.RawDeliveryEnv or RawDeliveryFile. It never carries a value.
func (h *Holder) Deliveries() map[string]string {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]string, len(h.held))
	for name, secret := range h.held {
		out[name] = secret.delivery
	}
	return out
}

// Sweep removes every file in the directory: values a previous run of the
// agent kept are not trusted to still stand, so a start holds none until the
// pool answers again.
func (h *Holder) Sweep() error {
	entries, err := os.ReadDir(h.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(h.dir, entry.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// SetApplied tells the Holder the raw secrets the applied runtime-config
// document names, variable to delivery. A change — the first one included —
// asks the pool at once; an empty set removes everything held. A nil Holder
// is told nothing.
func (h *Holder) SetApplied(set map[string]string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.told && sameSet(h.applied, set) {
		return
	}
	h.applied = make(map[string]string, len(set))
	for name, delivery := range set {
		h.applied[name] = delivery
	}
	h.told = true
	h.gen++
	h.genAt = h.now()
	h.settled = make(chan struct{})
	if len(h.applied) == 0 {
		for name := range h.held {
			if err := h.removeLocked(name); err != nil {
				h.logger.Warn("remove raw secret", "name", name, "error", err)
			}
		}
		h.settleLocked(h.gen)
		return
	}
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *Holder) settleLocked(gen int64) {
	if gen != h.gen || h.fetchedGen == gen {
		return
	}
	h.fetchedGen = gen
	close(h.settled)
}

// AwaitFetched returns once the pool has answered for the set last applied,
// or at once when nothing is applied or the set is empty. It waits at most
// until AwaitBound after the set changed, then returns, logging what it went
// on without.
func (h *Holder) AwaitFetched(ctx context.Context) error {
	if h == nil {
		return nil
	}
	h.mu.RLock()
	if !h.told || h.fetchedGen == h.gen {
		h.mu.RUnlock()
		return nil
	}
	settled := h.settled
	wait := h.genAt.Add(AwaitBound).Sub(h.now())
	h.mu.RUnlock()
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-settled:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		h.logger.Warn("the pool has not answered for the sandbox's raw secrets; going on without them", "waited", AwaitBound)
		return nil
	}
}

// Run asks the pool whenever the applied set changes, and while it is not
// empty every PollInterval and again before a value expires, until ctx ends.
// A failed ask keeps what is held until it expires. Every file is removed
// when Run returns: the values are the running agent's to keep, and a stopped
// one keeps none.
func (h *Holder) Run(ctx context.Context, fetch Fetch) {
	defer h.clear()
	for {
		h.mu.RLock()
		active, gen := len(h.applied) > 0, h.gen
		h.mu.RUnlock()
		var timer *time.Timer
		var due <-chan time.Time
		if active {
			wait := PollInterval
			resp, err := fetch(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				h.logger.Warn("ask the pool for raw secrets", "error", err)
				h.expire()
				wait = retryInterval
			} else if err := h.apply(resp, gen); err != nil {
				h.logger.Warn("keep raw secrets", "error", err)
			}
			if next := h.untilRenewal(); next < wait {
				wait = next
			}
			timer = time.NewTimer(wait)
			due = timer.C
		}
		select {
		case <-ctx.Done():
		case <-h.wake:
		case <-due:
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// apply makes the files what resp, the pool's answer for applied set gen,
// says the sandbox holds: each written when its value moved, and each one resp
// no longer names removed. Only what the applied set names, as it names it,
// is kept: the document is what the sandbox was told it holds, and a value for
// anything else is dropped. An invalid entry is skipped, and the rest applied.
func (h *Holder) apply(resp sandboxconfig.RawSecretsResponse, gen int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.settleLocked(gen)
	var errs []error
	want := map[string]held{}
	for _, secret := range resp.Secrets {
		if !validName(secret.Name) || !sandboxconfig.ValidRawDelivery(secret.Delivery) {
			errs = append(errs, fmt.Errorf("raw secret %q: invalid name or delivery %q", secret.Name, secret.Delivery))
			continue
		}
		if h.applied[secret.Name] != secret.Delivery {
			errs = append(errs, fmt.Errorf("raw secret %q: not delivered %s by the applied document", secret.Name, secret.Delivery))
			continue
		}
		want[secret.Name] = held{delivery: secret.Delivery, value: secret.Value, expiresAt: secret.ExpiresAt}
	}
	for name := range h.held {
		if _, ok := want[name]; !ok {
			errs = append(errs, h.removeLocked(name))
		}
	}
	if len(want) > 0 {
		if err := h.ensureDir(); err != nil {
			// Nothing is written anywhere but memory: without the
			// directory, nothing is held.
			for name := range h.held {
				errs = append(errs, h.removeLocked(name))
			}
			return errors.Join(append(errs, err)...)
		}
	}
	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		next := want[name]
		if current, ok := h.held[name]; ok && current.value == next.value {
			h.held[name] = next
			continue
		}
		if err := h.write(name, next.value); err != nil {
			errs = append(errs, err, h.removeLocked(name))
			continue
		}
		h.held[name] = next
	}
	return errors.Join(errs...)
}

// expire removes what has expired, for an ask that failed.
func (h *Holder) expire() {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	for name, secret := range h.held {
		if secret.expiresAt != nil && !now.Before(*secret.expiresAt) {
			if err := h.removeLocked(name); err != nil {
				h.logger.Warn("remove expired raw secret", "name", name, "error", err)
			}
		}
	}
}

// untilRenewal is how long until the earliest held value should be asked for
// again.
func (h *Holder) untilRenewal() time.Duration {
	h.mu.RLock()
	defer h.mu.RUnlock()
	next := PollInterval
	now := h.now()
	for _, secret := range h.held {
		if secret.expiresAt == nil {
			continue
		}
		wait := secret.expiresAt.Sub(now) - expirySkew
		if wait < time.Second {
			wait = time.Second
		}
		if wait < next {
			next = wait
		}
	}
	return next
}

func (h *Holder) clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for name := range h.held {
		if err := h.removeLocked(name); err != nil {
			h.logger.Warn("remove raw secret", "name", name, "error", err)
		}
	}
}

func (h *Holder) removeLocked(name string) error {
	delete(h.held, name)
	if err := os.Remove(h.Path(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove raw secret %s: %w", name, err)
	}
	return nil
}

// ensureDir makes the directory, 0700 and the sandbox user's, and refuses one
// that is not in memory: a value is never written to a disk.
func (h *Holder) ensureDir() error {
	if err := os.MkdirAll(h.dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", h.dir, err)
	}
	if err := h.onMemory(h.dir); err != nil {
		return fmt.Errorf("refuse to keep raw secrets in %s: %w", h.dir, err)
	}
	if err := os.Chmod(h.dir, 0o700); err != nil {
		return fmt.Errorf("chmod %s: %w", h.dir, err)
	}
	return h.chown(h.dir)
}

// write replaces name's file with value, 0400 and the sandbox user's, by a
// rename inside the directory so a reader never sees half a value.
func (h *Holder) write(name, value string) error {
	tmp, err := os.CreateTemp(h.dir, "."+name+".*")
	if err != nil {
		return fmt.Errorf("write raw secret %s: %w", name, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(value); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write raw secret %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write raw secret %s: %w", name, err)
	}
	if err := os.Chmod(tmp.Name(), 0o400); err != nil {
		return fmt.Errorf("chmod raw secret %s: %w", name, err)
	}
	if err := h.chown(tmp.Name()); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), h.Path(name)); err != nil {
		return fmt.Errorf("write raw secret %s: %w", name, err)
	}
	return nil
}

func (h *Holder) chown(path string) error {
	if h.owner.UID == nil || h.owner.GID == nil {
		return nil
	}
	if err := os.Chown(path, int(*h.owner.UID), int(*h.owner.GID)); err != nil {
		return fmt.Errorf("chown %s: %w", path, err)
	}
	return nil
}

func sameSet(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, delivery := range a {
		if other, ok := b[name]; !ok || other != delivery {
			return false
		}
	}
	return true
}

// validName is a variable name: what a binding's env name may be, and so
// one path segment.
func validName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
