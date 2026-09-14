// Package resume makes an execstream connection survive replacement of its
// underlying transport without losing or duplicating process actions.
//
// The protocol follows the position-and-retransmit model used by RSocket
// resumption: a logical session has an opaque token, every non-idempotent action
// has a monotonically increasing position, the host cumulatively acknowledges
// positions only after applying them, and the client retains unacknowledged
// actions for retransmission. Resize and Ready are connection state rather than
// actions: the latest resize is coalesced and Ready is restored per connection.
// Repaint is neither: it goes out on the current connection only and is not
// retained, because a reconnect repaints on its own.
//
// Positions are only meaningful to the process that applied them. Every host
// stream has a random instance identity that SessionOK carries, so a client
// reconnecting to the same exec id after its process was replaced — a terminal
// revived in place — starts over on the new process instead of resuming
// positions it never saw.
package resume

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/discobox-ai/discobox/execstream/frame"
)

const (
	positionSize    = 8
	actionHeaderLen = positionSize + 1
	tokenSize       = 32
	instanceSize    = 16
	sessionSize     = 2*positionSize + tokenSize + instanceSize
	sessionOKSize   = positionSize + instanceSize
)

// instance identifies one host stream, and so one process. The zero value is a
// client that has not yet established a session with any host.
type instance [instanceSize]byte

var (
	// ErrRejected means the peer no longer has enough logical-session state to
	// resume without risking lost or duplicate actions.
	ErrRejected = errors.New("exec stream resume rejected")
	// ErrProtocol means a peer sent an invalid resumable-stream frame.
	ErrProtocol = errors.New("invalid exec stream resume protocol")
)

type sessionRequest struct {
	token []byte
	// firstAvailable is the oldest action position the client can still
	// retransmit: one past its last acknowledgement.
	firstAvailable uint64
	// accepted is the newest action position the client has assigned. A host
	// replacing the process the client last spoke to starts the session here.
	accepted uint64
	// instance is the host the client last established with, or zero.
	instance instance
}

type action struct {
	position uint64
	frame    frame.Frame
}

func encodeSession(request sessionRequest) ([]byte, error) {
	if len(request.token) != tokenSize {
		return nil, fmt.Errorf("%w: session token is %d bytes, want %d", ErrProtocol, len(request.token), tokenSize)
	}
	if err := validateSession(request); err != nil {
		return nil, err
	}
	payload := make([]byte, sessionSize)
	binary.BigEndian.PutUint64(payload[:positionSize], request.firstAvailable)
	binary.BigEndian.PutUint64(payload[positionSize:2*positionSize], request.accepted)
	copy(payload[2*positionSize:2*positionSize+tokenSize], request.token)
	copy(payload[2*positionSize+tokenSize:], request.instance[:])
	return payload, nil
}

// EncodeNewSession encodes the handshake of a client that has never
// established a session: no host instance and no actions yet.
func EncodeNewSession(token []byte) ([]byte, error) {
	return encodeSession(sessionRequest{token: token, firstAvailable: 1})
}

func decodeSession(payload []byte) (sessionRequest, error) {
	if len(payload) != sessionSize {
		return sessionRequest{}, fmt.Errorf("%w: session payload is %d bytes, want %d", ErrProtocol, len(payload), sessionSize)
	}
	request := sessionRequest{
		firstAvailable: binary.BigEndian.Uint64(payload[:positionSize]),
		accepted:       binary.BigEndian.Uint64(payload[positionSize : 2*positionSize]),
		token:          append([]byte(nil), payload[2*positionSize:2*positionSize+tokenSize]...),
	}
	copy(request.instance[:], payload[2*positionSize+tokenSize:])
	if err := validateSession(request); err != nil {
		return sessionRequest{}, err
	}
	return request, nil
}

func validateSession(request sessionRequest) error {
	if request.firstAvailable == 0 {
		return fmt.Errorf("%w: first available position is zero", ErrProtocol)
	}
	if request.accepted < request.firstAvailable-1 {
		return fmt.Errorf("%w: accepted position %d precedes acknowledged position %d", ErrProtocol, request.accepted, request.firstAvailable-1)
	}
	return nil
}

func encodeSessionOK(position uint64, host instance) []byte {
	payload := make([]byte, sessionOKSize)
	binary.BigEndian.PutUint64(payload[:positionSize], position)
	copy(payload[positionSize:], host[:])
	return payload
}

func decodeSessionOK(payload []byte) (uint64, instance, error) {
	if len(payload) != sessionOKSize {
		return 0, instance{}, fmt.Errorf("%w: session acknowledgement payload is %d bytes, want %d", ErrProtocol, len(payload), sessionOKSize)
	}
	var host instance
	copy(host[:], payload[positionSize:])
	if host == (instance{}) {
		return 0, instance{}, fmt.Errorf("%w: host instance is zero", ErrProtocol)
	}
	return binary.BigEndian.Uint64(payload[:positionSize]), host, nil
}

// DecodeSessionOK decodes the host position a SessionOK frame reports.
func DecodeSessionOK(payload []byte) (uint64, error) {
	position, _, err := decodeSessionOK(payload)
	return position, err
}

func encodePosition(position uint64) []byte {
	payload := make([]byte, positionSize)
	binary.BigEndian.PutUint64(payload, position)
	return payload
}

// EncodePosition encodes a cumulative Ack position.
func EncodePosition(position uint64) []byte { return encodePosition(position) }

func decodePosition(payload []byte) (uint64, error) {
	if len(payload) != positionSize {
		return 0, fmt.Errorf("%w: position payload is %d bytes, want %d", ErrProtocol, len(payload), positionSize)
	}
	return binary.BigEndian.Uint64(payload), nil
}

// DecodePosition decodes a cumulative Ack position.
func DecodePosition(payload []byte) (uint64, error) { return decodePosition(payload) }

func encodeAction(position uint64, typ byte, payload []byte) ([]byte, error) {
	if position == 0 {
		return nil, fmt.Errorf("%w: action position is zero", ErrProtocol)
	}
	if !isActionType(typ) {
		return nil, fmt.Errorf("%w: frame type %d is not a resumable action", ErrProtocol, typ)
	}
	out := make([]byte, actionHeaderLen+len(payload))
	binary.BigEndian.PutUint64(out[:positionSize], position)
	out[positionSize] = typ
	copy(out[actionHeaderLen:], payload)
	return out, nil
}

// EncodeAction encodes one positioned process action.
func EncodeAction(position uint64, typ byte, payload []byte) ([]byte, error) {
	return encodeAction(position, typ, payload)
}

func decodeAction(payload []byte) (action, error) {
	if len(payload) < actionHeaderLen {
		return action{}, fmt.Errorf("%w: action payload is %d bytes, want at least %d", ErrProtocol, len(payload), actionHeaderLen)
	}
	position := binary.BigEndian.Uint64(payload[:positionSize])
	typ := payload[positionSize]
	if position == 0 {
		return action{}, fmt.Errorf("%w: action position is zero", ErrProtocol)
	}
	if !isActionType(typ) {
		return action{}, fmt.Errorf("%w: frame type %d is not a resumable action", ErrProtocol, typ)
	}
	return action{
		position: position,
		frame: frame.Frame{
			Type:    typ,
			Payload: append([]byte(nil), payload[actionHeaderLen:]...),
		},
	}, nil
}

func isActionType(typ byte) bool {
	switch typ {
	case frame.Input, frame.Signal, frame.CloseInput:
		return true
	default:
		return false
	}
}

// IsActionType reports whether typ has process effects that require positioned,
// acknowledged delivery on a resumable session.
func IsActionType(typ byte) bool { return isActionType(typ) }
