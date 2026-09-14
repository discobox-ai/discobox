//go:build windows

package wslcsession

import (
	"errors"
	"testing"
)

// collidingManager holds its name for the first collisions CreateSession calls,
// the way the service does for a session a killed process left behind.
type collidingManager struct {
	session     *fakeSession
	collisions  int
	creates     int
	terminates  int
	terminateOK bool
}

func (m *collidingManager) CreateSession(Options) (wslcSession, error) {
	m.creates++
	if m.creates <= m.collisions {
		code := hrErrorAlreadyExists
		return nil, hresultError(int32(code))
	}
	return m.session, nil
}

func (m *collidingManager) TerminateExisting(Options) error {
	m.terminates++
	if !m.terminateOK {
		return errors.New("terminate refused")
	}
	return nil
}

func (m *collidingManager) Release() {}

func useCollidingManager(t *testing.T, m *collidingManager) {
	t.Helper()
	previous := activateManager
	activateManager = func() (sessionManager, error) { return m, nil }
	t.Cleanup(func() { activateManager = previous })
}

// A name held by a session the caller owns the name of is ended and taken over,
// once: the development restart that used to wait on the service to notice a
// killed server now boots at once.
func TestReplaceExistingEndsTheSessionHoldingTheName(t *testing.T) {
	m := &collidingManager{session: &fakeSession{}, collisions: 1, terminateOK: true}
	useCollidingManager(t, m)

	session, err := NewSession(Options{DisplayName: "discobox-pool_1", ReplaceExisting: true})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = session.Close() }()
	if m.terminates != 1 || m.creates != 2 {
		t.Fatalf("terminates=%d creates=%d, want the holder ended once and the session created after it", m.terminates, m.creates)
	}
	if !session.ReplacedExisting() {
		t.Fatal("ReplacedExisting = false for a session that ended its predecessor")
	}
}

// Without the option a taken name stays an error, and nothing is ended: the
// service hands back any session of that name, a live process's included, so
// ending one is only ever the caller's explicit choice.
func TestTakenNameIsAnErrorWithoutReplaceExisting(t *testing.T) {
	m := &collidingManager{session: &fakeSession{}, collisions: 1, terminateOK: true}
	useCollidingManager(t, m)

	_, err := NewSession(Options{DisplayName: "discobox-pool_1"})
	if !errors.Is(err, ErrSessionExists) {
		t.Fatalf("error = %v, want ErrSessionExists", err)
	}
	if m.terminates != 0 {
		t.Fatalf("terminates = %d, want nothing ended without ReplaceExisting", m.terminates)
	}
}

// A holder that cannot be ended, or a name still taken after it was, is the
// collision it would have been - so the caller's wait-and-explain path still
// applies rather than a failure it has never seen.
func TestReplaceExistingThatCannotEndItReportsTheCollision(t *testing.T) {
	for name, m := range map[string]*collidingManager{
		"terminate fails":           {session: &fakeSession{}, collisions: 1, terminateOK: false},
		"name still taken after it": {session: &fakeSession{}, collisions: 2, terminateOK: true},
	} {
		t.Run(name, func(t *testing.T) {
			useCollidingManager(t, m)
			_, err := NewSession(Options{DisplayName: "discobox-pool_1", ReplaceExisting: true})
			if !errors.Is(err, ErrSessionExists) {
				t.Fatalf("error = %v, want ErrSessionExists", err)
			}
			if m.terminates != 1 {
				t.Fatalf("terminates = %d, want one attempt, not a loop", m.terminates)
			}
		})
	}
}
