// Package statuswait is the version behind GET .../status/wait: each part of
// the status that wakes the pool (ADR 26-11-03-815, ADR 26-11-09-062) keeps a
// Signal, and Wait holds a caller on all of them at once.
package statuswait

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Hold is how long Wait holds a caller naming the current version before
// answering with it unchanged. It is the agent's bound, never the caller's, and
// sits under the idle timeout of the common proxies a pool may be reached
// through.
const Hold = 25 * time.Second

// Signal counts the changes to one thing the status reports and wakes whoever
// is waiting on the next. Its zero value is ready to use, and a nil Signal is
// one that never changes.
type Signal struct {
	mu sync.Mutex
	// epoch and generation make the version: the generation counts changes,
	// and the epoch, drawn on first use, keeps a restarted agent's count from
	// being mistaken for an unchanged one.
	epoch      string
	generation uint64
	// changed is closed and replaced on every change, waking every Wait held
	// on the version before it.
	changed chan struct{}
}

// Changed records a change: the version moves and every held Wait answers.
func (s *Signal) Changed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	s.generation++
	close(s.changed)
	s.changed = make(chan struct{})
}

// Version names the current state: it moves whenever Changed is called, and is
// opaque to everyone but Wait.
func (s *Signal) Version() string {
	version, _ := s.current()
	return version
}

// current is the version and the channel closed when it next moves.
func (s *Signal) current() (string, <-chan struct{}) {
	if s == nil {
		return "", nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initLocked()
	return s.epoch + "-" + strconv.FormatUint(s.generation, 10), s.changed
}

func (s *Signal) initLocked() {
	if s.changed != nil {
		return
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	s.epoch = hex.EncodeToString(b[:])
	s.changed = make(chan struct{})
}

// Version joins the signals' versions into one; it moves whenever any of
// theirs does. A nil signal contributes an empty part, and no signal at all
// makes an empty version, which the pool takes for an agent with nothing to
// wait on.
func Version(signals ...*Signal) string {
	version, _ := current(signals)
	return version
}

func current(signals []*Signal) (string, []<-chan struct{}) {
	parts := make([]string, len(signals))
	changed := make([]<-chan struct{}, 0, len(signals))
	some := false
	for i, s := range signals {
		v, c := s.current()
		parts[i] = v
		if c != nil {
			some = true
			changed = append(changed, c)
		}
	}
	if !some {
		return "", nil
	}
	return strings.Join(parts, "."), changed
}

// Wait answers with the joined version, at once unless after is that version:
// then it holds until any signal changes, hold passes or ctx ends, and answers
// with whatever the version is then. Waiting is the caller's to ask for by
// naming the version it has; one that names none, or an old one, is answered
// straight away (ADR 26-11-03-815 §2).
func Wait(ctx context.Context, after string, hold time.Duration, signals ...*Signal) string {
	version, changed := current(signals)
	if version == "" || after != version {
		return version
	}
	timer := time.NewTimer(hold)
	defer timer.Stop()
	// Any one of them answers. A goroutine per signal is fine at the two the
	// status has; each ends with the wait.
	woken := make(chan struct{}, 1)
	done := make(chan struct{})
	defer close(done)
	for _, c := range changed {
		go func() {
			select {
			case <-c:
				select {
				case woken <- struct{}{}:
				default:
				}
			case <-done:
			}
		}()
	}
	select {
	case <-woken:
	case <-timer.C:
	case <-ctx.Done():
	}
	return Version(signals...)
}
