package resume

import (
	"crypto/rand"
	"fmt"
	"sync"

	"github.com/discobox-ai/discobox/execstream/frame"
)

// Server retains the applied input position of every logical session for one
// hosted process. Its lifetime must therefore match that process's stream: its
// instance is what tells a reconnecting client whether it reached the process
// its positions describe.
type Server struct {
	instance instance
	mu       sync.Mutex
	sessions map[string]*serverSession
	clock    uint64
}

// MaxSessions bounds retained logical-session positions for one process. A
// disconnected session remains resumable until this cache needs its
// least-recently-used inactive entry. Active sessions are never evicted.
const MaxSessions = 64

type serverSession struct {
	mu       sync.Mutex
	position uint64
	active   int
	lastUsed uint64
}

// Receiver is one physical connection's view of a logical session.
type Receiver struct {
	server    *Server
	session   *serverSession
	closeOnce sync.Once
}

func NewServer() *Server {
	s := &Server{sessions: map[string]*serverSession{}}
	_, _ = rand.Read(s.instance[:])
	return s
}

// Accept opens or resumes a logical session. It returns the SessionOK payload
// to send the client: the highest action already applied and this host's
// instance.
//
// A client whose last session was with another instance is starting over on a
// replaced process, not resuming this one. Nothing it retained was applied
// here, so its session begins at the client's newest accepted position and the
// client abandons what it retained.
func (s *Server) Accept(payload []byte) (*Receiver, []byte, error) {
	request, err := decodeSession(payload)
	if err != nil {
		return nil, nil, err
	}
	replaced := request.instance != (instance{}) && request.instance != s.instance

	key := string(request.token)
	s.mu.Lock()
	session := s.sessions[key]
	if session == nil {
		if !replaced && request.firstAvailable != 1 {
			s.mu.Unlock()
			return nil, nil, fmt.Errorf("%w: host has no session state before position %d", ErrRejected, request.firstAvailable)
		}
		if len(s.sessions) >= MaxSessions {
			if !s.evictInactiveLocked() {
				s.mu.Unlock()
				return nil, nil, fmt.Errorf("%w: host already serves %d active logical sessions", ErrRejected, MaxSessions)
			}
		}
		session = &serverSession{}
		s.sessions[key] = session
	}
	session.active++
	s.touchLocked(session)
	s.mu.Unlock()

	receiver := &Receiver{server: s, session: session}
	session.mu.Lock()
	defer session.mu.Unlock()
	if replaced {
		// The session may already exist from a handshake to this instance that
		// failed before the client adopted it. The client has sent this host no
		// actions since, and may have accepted more while disconnected.
		session.position = max(session.position, request.accepted)
	}
	if session.position+1 < request.firstAvailable {
		receiver.Close()
		return nil, nil, fmt.Errorf("%w: host position %d precedes client's first available position %d", ErrRejected, session.position, request.firstAvailable)
	}
	return receiver, encodeSessionOK(session.position, s.instance), nil
}

// Apply applies one positioned action at most once and returns the cumulative
// position to acknowledge. apply runs while the logical session is serialized,
// so concurrent old and replacement connections cannot reorder actions.
func (r *Receiver) Apply(payload []byte, apply func(frame.Frame) error) (uint64, error) {
	next, err := decodeAction(payload)
	if err != nil {
		return 0, err
	}

	r.session.mu.Lock()
	defer r.session.mu.Unlock()
	if next.position <= r.session.position {
		return r.session.position, nil
	}
	if next.position != r.session.position+1 {
		return r.session.position, fmt.Errorf("%w: received action position %d after %d", ErrProtocol, next.position, r.session.position)
	}
	if apply != nil {
		if err := apply(next.frame); err != nil {
			return r.session.position, err
		}
	}
	r.session.position = next.position
	return r.session.position, nil
}

// Close releases this physical connection's claim on the logical session.
// The position remains cached for reconnect until an inactive session must be
// evicted to make room for a new logical session.
func (r *Receiver) Close() {
	if r == nil || r.server == nil || r.session == nil {
		return
	}
	r.closeOnce.Do(func() {
		r.server.mu.Lock()
		if r.session.active > 0 {
			r.session.active--
		}
		r.server.touchLocked(r.session)
		r.server.mu.Unlock()
	})
}

func (s *Server) touchLocked(session *serverSession) {
	s.clock++
	session.lastUsed = s.clock
}

func (s *Server) evictInactiveLocked() bool {
	var oldestKey string
	var oldest *serverSession
	for key, session := range s.sessions {
		if session.active != 0 || oldest != nil && session.lastUsed >= oldest.lastUsed {
			continue
		}
		oldestKey, oldest = key, session
	}
	if oldest == nil {
		return false
	}
	delete(s.sessions, oldestKey)
	return true
}
