package resume

import (
	"fmt"
	"sync"

	"github.com/discobox-ai/discobox/execstream/frame"
)

// Server retains the applied input position of every logical session for one
// hosted process. Its lifetime must therefore match that process's stream.
type Server struct {
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
	return &Server{sessions: map[string]*serverSession{}}
}

// Accept opens or resumes a logical session. position is the highest action
// already applied and must be returned to the client in a SessionOK frame.
func (s *Server) Accept(payload []byte) (*Receiver, uint64, error) {
	request, err := decodeSession(payload)
	if err != nil {
		return nil, 0, err
	}

	key := string(request.token)
	s.mu.Lock()
	session := s.sessions[key]
	if session == nil {
		if request.firstAvailable != 1 {
			s.mu.Unlock()
			return nil, 0, fmt.Errorf("%w: host has no session state before position %d", ErrRejected, request.firstAvailable)
		}
		if len(s.sessions) >= MaxSessions {
			if !s.evictInactiveLocked() {
				s.mu.Unlock()
				return nil, 0, fmt.Errorf("%w: host already serves %d active logical sessions", ErrRejected, MaxSessions)
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
	if session.position+1 < request.firstAvailable {
		receiver.Close()
		return nil, 0, fmt.Errorf("%w: host position %d precedes client's first available position %d", ErrRejected, session.position, request.firstAvailable)
	}
	return receiver, session.position, nil
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
