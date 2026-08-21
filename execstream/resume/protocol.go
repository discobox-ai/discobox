// Package resume makes an execstream connection survive replacement of its
// underlying transport without losing or duplicating process actions.
//
// The protocol follows the position-and-retransmit model used by RSocket
// resumption: a logical session has an opaque token, every non-idempotent action
// has a monotonically increasing position, the host cumulatively acknowledges
// positions only after applying them, and the client retains unacknowledged
// actions for retransmission. Resize and Ready are connection state rather than
// actions: the latest resize is coalesced and Ready is restored per connection.
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
)

var (
	// ErrRejected means the peer no longer has enough logical-session state to
	// resume without risking lost or duplicate actions.
	ErrRejected = errors.New("exec stream resume rejected")
	// ErrProtocol means a peer sent an invalid resumable-stream frame.
	ErrProtocol = errors.New("invalid exec stream resume protocol")
)

type sessionRequest struct {
	token          []byte
	firstAvailable uint64
}

type action struct {
	position uint64
	frame    frame.Frame
}

func encodeSession(token []byte, firstAvailable uint64) ([]byte, error) {
	if len(token) != tokenSize {
		return nil, fmt.Errorf("%w: session token is %d bytes, want %d", ErrProtocol, len(token), tokenSize)
	}
	if firstAvailable == 0 {
		return nil, fmt.Errorf("%w: first available position is zero", ErrProtocol)
	}
	payload := make([]byte, positionSize+len(token))
	binary.BigEndian.PutUint64(payload[:positionSize], firstAvailable)
	copy(payload[positionSize:], token)
	return payload, nil
}

// EncodeSession encodes the handshake for a logical session.
func EncodeSession(token []byte, firstAvailable uint64) ([]byte, error) {
	return encodeSession(token, firstAvailable)
}

func decodeSession(payload []byte) (sessionRequest, error) {
	if len(payload) != positionSize+tokenSize {
		return sessionRequest{}, fmt.Errorf("%w: session payload is %d bytes, want %d", ErrProtocol, len(payload), positionSize+tokenSize)
	}
	firstAvailable := binary.BigEndian.Uint64(payload[:positionSize])
	if firstAvailable == 0 {
		return sessionRequest{}, fmt.Errorf("%w: first available position is zero", ErrProtocol)
	}
	return sessionRequest{
		token:          append([]byte(nil), payload[positionSize:]...),
		firstAvailable: firstAvailable,
	}, nil
}

func encodePosition(position uint64) []byte {
	payload := make([]byte, positionSize)
	binary.BigEndian.PutUint64(payload, position)
	return payload
}

// EncodePosition encodes a cumulative SessionOK or Ack position.
func EncodePosition(position uint64) []byte { return encodePosition(position) }

func decodePosition(payload []byte) (uint64, error) {
	if len(payload) != positionSize {
		return 0, fmt.Errorf("%w: position payload is %d bytes, want %d", ErrProtocol, len(payload), positionSize)
	}
	return binary.BigEndian.Uint64(payload), nil
}

// DecodePosition decodes a cumulative SessionOK or Ack position.
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
