package audit

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/discobox/pkg/wsrecord"
)

const (
	defaultUpgradeStreamQueueSize = 1024
	UpgradeStreamFormatRawFrames  = "discobox-upgrade-stream-v1"

	streamFrameTypeData    = 1
	streamFrameTypeSummary = 2

	// streamDataFrameOverhead is a data frame's type, timestamp, direction,
	// and payload length; streamSummaryFrameSize is the whole summary frame.
	streamDataFrameOverhead = 1 + 8 + 1 + 4
	streamSummaryFrameSize  = 1 + 8 + 4*8

	StreamClientToServer StreamDirection = 1
	StreamServerToClient StreamDirection = 2
)

var upgradeStreamFileMagic = [4]byte{'D', 'B', 'S', '1'}

// StreamDirection identifies the direction of a recorded upgraded stream chunk.
type StreamDirection byte

// StreamRecord describes a raw upgraded stream spool file.
type StreamRecord struct {
	SessionID string
	File      string
	Format    string
}

// StreamSession asynchronously records raw upgraded-stream bytes to disk.
type StreamSession struct {
	writer io.WriteCloser
	events chan streamEvent
	done   chan struct{}
	wg     *sync.WaitGroup

	closeOnce sync.Once
	stateMu   sync.RWMutex
	closed    bool
	// onClose releases the recorder's hold on this spool file. It runs after
	// the last frame is flushed, so the retention sweep cannot reclaim the
	// file while anything is still being written to it.
	onClose func()
	// onWrite counts written frames toward the recorder's spool budget.
	onWrite func(int64)
	// patterns are the client's raw secrets, kept out of every chunk
	// (Recorder.SetRedactor).
	patterns    [][]byte
	websocket   bool
	extensions  string
	queuedBytes atomic.Int64
	captureLost atomic.Bool

	droppedChunks atomic.Uint64
	droppedBytes  atomic.Uint64
	clientBytes   atomic.Uint64
	serverBytes   atomic.Uint64
}

type streamEvent struct {
	direction byte
	at        time.Time
	payload   []byte
}

// BeginUpgradeStream creates a raw upgraded stream spool file.
func beginUpgradeStream(dir, clientID, upgradeType, extensions string, queueSize int, wg *sync.WaitGroup) (*StreamRecord, *StreamSession, error) {
	if dir == "" {
		return nil, nil, nil
	}
	if queueSize <= 0 {
		queueSize = defaultUpgradeStreamQueueSize
	}
	sessionID := generateStreamID()
	relativePath := filepath.Join("streams", safePath(clientID), fmt.Sprintf("stream-%s.bin", sessionID))
	filePath := filepath.Join(dir, relativePath)
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create stream spool dir: %w", err)
	}
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("create stream spool: %w", err)
	}
	isWebSocket := strings.EqualFold(upgradeType, "websocket")
	var headerErr error
	if isWebSocket {
		headerErr = wsrecord.WriteHeader(file)
	} else {
		headerErr = writeUpgradeStreamHeader(file, sessionID, upgradeType, time.Now().UTC())
	}
	if headerErr != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("write stream header: %w", headerErr)
	}
	record := &StreamRecord{
		SessionID: sessionID,
		File:      filepath.ToSlash(relativePath),
		Format:    UpgradeStreamFormatRawFrames,
	}
	session := newStreamSession(file, queueSize, wg)
	session.websocket = isWebSocket
	session.extensions = extensions
	if isWebSocket {
		record.Format = wsrecord.Format
	}
	return record, session, nil
}

func newStreamSession(writer io.WriteCloser, queueSize int, wg *sync.WaitGroup) *StreamSession {
	if wg != nil {
		wg.Add(1)
	}
	session := &StreamSession{
		writer: writer,
		events: make(chan streamEvent, queueSize),
		done:   make(chan struct{}),
		wg:     wg,
	}
	return session
}

// RecordChunk queues an upgraded-stream payload for asynchronous spooling.
func (s *StreamSession) RecordChunk(direction StreamDirection, payload []byte) {
	if s == nil || len(payload) == 0 {
		return
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.closed {
		return
	}
	// Once bytes are missing, framing and compression state are no longer
	// trustworthy. Stop capturing this socket instead of inventing messages.
	if s.websocket && (s.captureLost.Load() || s.queuedBytes.Load()+int64(len(payload)) > 8<<20) {
		s.captureLost.Store(true)
		s.droppedChunks.Add(1)
		s.droppedBytes.Add(uint64(len(payload)))
		return
	}
	copied := bytes.Clone(payload)
	if !s.websocket {
		copied = redactPatterns(copied, s.patterns)
	}
	event := streamEvent{direction: byte(direction), at: time.Now().UTC(), payload: copied}
	s.queuedBytes.Add(int64(len(copied)))
	select {
	case s.events <- event:
	default:
		s.queuedBytes.Add(-int64(len(copied)))
		if s.websocket {
			s.captureLost.Store(true)
		}
		s.droppedChunks.Add(1)
		s.droppedBytes.Add(uint64(len(payload)))
	}
}

// Close flushes queued stream frames and closes the spool file.
func (s *StreamSession) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.stateMu.Lock()
		defer s.stateMu.Unlock()
		s.closed = true
		close(s.events)
	})
	<-s.done
	if s.onClose != nil {
		s.onClose()
	}
	return nil
}

func (s *StreamSession) ClientBytes() uint64 {
	if s == nil {
		return 0
	}
	return s.clientBytes.Load()
}

func (s *StreamSession) ServerBytes() uint64 {
	if s == nil {
		return 0
	}
	return s.serverBytes.Load()
}

func (s *StreamSession) DroppedChunks() uint64 {
	if s == nil {
		return 0
	}
	return s.droppedChunks.Load()
}

func (s *StreamSession) DroppedBytes() uint64 {
	if s == nil {
		return 0
	}
	return s.droppedBytes.Load()
}

func (s *StreamSession) run() {
	defer close(s.done)
	if s.websocket {
		s.runWebSocket()
		return
	}
	for event := range s.events {
		s.queuedBytes.Add(-int64(len(event.payload)))
		if err := writeUpgradeStreamDataFrame(s.writer, event.direction, time.Now().UTC(), event.payload); err != nil {
			s.droppedChunks.Add(1)
			s.droppedBytes.Add(uint64(len(event.payload)))
			continue
		}
		if s.onWrite != nil {
			s.onWrite(streamDataFrameOverhead + int64(len(event.payload)))
		}
		switch event.direction {
		case byte(StreamClientToServer):
			s.clientBytes.Add(uint64(len(event.payload)))
		case byte(StreamServerToClient):
			s.serverBytes.Add(uint64(len(event.payload)))
		}
	}
	_ = writeUpgradeStreamSummaryFrame(s.writer, time.Now().UTC(), s)
	_ = s.writer.Close()
	if s.wg != nil {
		s.wg.Done()
	}
}

func writeUpgradeStreamHeader(w io.Writer, sessionID, upgradeType string, startedAt time.Time) error {
	if len(sessionID) > math.MaxUint16 {
		return fmt.Errorf("stream session id too long")
	}
	if len(upgradeType) > math.MaxUint16 {
		return fmt.Errorf("stream upgrade type too long")
	}
	if _, err := w.Write(upgradeStreamFileMagic[:]); err != nil {
		return err
	}
	if err := writeByte(w, 1); err != nil {
		return err
	}
	if err := writeInt64(w, startedAt.UnixNano()); err != nil {
		return err
	}
	if err := writeUint16(w, uint16(len(sessionID))); err != nil {
		return err
	}
	if _, err := io.WriteString(w, sessionID); err != nil {
		return err
	}
	if err := writeUint16(w, uint16(len(upgradeType))); err != nil {
		return err
	}
	_, err := io.WriteString(w, upgradeType)
	return err
}

func writeUpgradeStreamDataFrame(w io.Writer, direction byte, timestamp time.Time, payload []byte) error {
	if len(payload) > math.MaxUint32 {
		return fmt.Errorf("stream payload frame too large")
	}
	if err := writeByte(w, streamFrameTypeData); err != nil {
		return err
	}
	if err := writeInt64(w, timestamp.UnixNano()); err != nil {
		return err
	}
	if err := writeByte(w, direction); err != nil {
		return err
	}
	if err := writeUint32(w, uint32(len(payload))); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func writeUpgradeStreamSummaryFrame(w io.Writer, timestamp time.Time, session *StreamSession) error {
	if err := writeByte(w, streamFrameTypeSummary); err != nil {
		return err
	}
	if err := writeInt64(w, timestamp.UnixNano()); err != nil {
		return err
	}
	if err := writeUint64(w, session.clientBytes.Load()); err != nil {
		return err
	}
	if err := writeUint64(w, session.serverBytes.Load()); err != nil {
		return err
	}
	if err := writeUint64(w, session.droppedChunks.Load()); err != nil {
		return err
	}
	return writeUint64(w, session.droppedBytes.Load())
}

func writeByte(w io.Writer, value byte) error {
	_, err := w.Write([]byte{value})
	return err
}

func writeUint16(w io.Writer, value uint16) error {
	return binary.Write(w, binary.BigEndian, value)
}

func writeUint32(w io.Writer, value uint32) error {
	return binary.Write(w, binary.BigEndian, value)
}

func writeUint64(w io.Writer, value uint64) error {
	return binary.Write(w, binary.BigEndian, value)
}

func writeInt64(w io.Writer, value int64) error {
	return binary.Write(w, binary.BigEndian, value)
}

var streamIDCounter atomic.Uint64

func generateStreamID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), streamIDCounter.Add(1))
}

func safePath(value string) string {
	cleaned := filepath.Clean(value)
	if cleaned == "." || cleaned == string(filepath.Separator) {
		return "unknown"
	}
	return filepath.Base(cleaned)
}

// runWebSocket decodes before redaction: masking and compression otherwise
// hide raw values, and redacting wire bytes can corrupt frame boundaries.
func (s *StreamSession) runWebSocket() {
	var storageErr error
	emit := func(record wsrecord.Record) error {
		if storageErr != nil {
			return storageErr
		}
		record.Payload = redactPatterns(bytes.Clone(record.Payload), s.patterns)
		if len(record.Payload) > wsrecord.MaxPayload {
			record.Opcode = wsrecord.CaptureError
			record.Payload = []byte("redacted message exceeds capture limit")
		}
		if err := wsrecord.Write(s.writer, record); err != nil {
			storageErr = err
			s.captureLost.Store(true)
			return err
		}
		if s.onWrite != nil {
			s.onWrite(int64(wsrecord.RecordHeaderSize + len(record.Payload)))
		}
		return nil
	}
	decoders, err := websocketDecoders(s.extensions, emit)
	if err != nil {
		for _, d := range decoders {
			d.fail(time.Now().UTC(), err.Error())
		}
	}
	for event := range s.events {
		s.queuedBytes.Add(-int64(len(event.payload)))
		if storageErr != nil {
			s.droppedChunks.Add(1)
			s.droppedBytes.Add(uint64(len(event.payload)))
			continue
		}
		switch event.direction {
		case byte(StreamClientToServer):
			s.clientBytes.Add(uint64(len(event.payload)))
			decoders[0].feed(event.payload, event.at)
		case byte(StreamServerToClient):
			s.serverBytes.Add(uint64(len(event.payload)))
			decoders[1].feed(event.payload, event.at)
		}
		if storageErr != nil {
			s.droppedChunks.Add(1)
			s.droppedBytes.Add(uint64(len(event.payload)))
		}
	}
	for _, d := range decoders {
		if s.captureLost.Load() {
			d.fail(time.Now().UTC(), "capture stopped: bytes dropped under load")
		} else if len(d.frame) > 0 || d.opcode != 0 {
			d.fail(time.Now().UTC(), "capture ended during a WebSocket message")
		}
	}
	_ = s.writer.Close()
	if s.wg != nil {
		s.wg.Done()
	}
}
