package audit

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultUpgradeStreamQueueSize = 1024
	UpgradeStreamFormatRawFrames  = "discobox-upgrade-stream-v1"

	streamFrameTypeData    = 1
	streamFrameTypeSummary = 2

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

	droppedChunks atomic.Uint64
	droppedBytes  atomic.Uint64
	clientBytes   atomic.Uint64
	serverBytes   atomic.Uint64
}

type streamEvent struct {
	direction byte
	payload   []byte
}

// BeginUpgradeStream creates a raw upgraded stream spool file.
func BeginUpgradeStream(dir, clientID, upgradeType string, queueSize int, wg *sync.WaitGroup) (*StreamRecord, *StreamSession, error) {
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
	if err := writeUpgradeStreamHeader(file, sessionID, upgradeType, time.Now().UTC()); err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("write stream header: %w", err)
	}
	record := &StreamRecord{
		SessionID: sessionID,
		File:      filepath.ToSlash(relativePath),
		Format:    UpgradeStreamFormatRawFrames,
	}
	return record, newStreamSession(file, queueSize, wg), nil
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
	go session.run()
	return session
}

// RecordChunk queues an upgraded-stream payload for asynchronous spooling.
func (s *StreamSession) RecordChunk(direction StreamDirection, payload []byte) {
	if s == nil || len(payload) == 0 {
		return
	}
	event := streamEvent{direction: byte(direction), payload: bytes.Clone(payload)}
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if s.closed {
		return
	}
	select {
	case s.events <- event:
	default:
		s.droppedChunks.Add(1)
		s.droppedBytes.Add(uint64(len(event.payload)))
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
	for event := range s.events {
		if err := writeUpgradeStreamDataFrame(s.writer, event.direction, time.Now().UTC(), event.payload); err != nil {
			s.droppedChunks.Add(1)
			s.droppedBytes.Add(uint64(len(event.payload)))
			continue
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
