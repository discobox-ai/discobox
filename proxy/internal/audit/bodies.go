package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	// BodyFormatRaw is the on-disk format for normal HTTP request/response bodies.
	BodyFormatRaw = "discobox-http-body-v1"

	BodyKindRequest  = "request"
	BodyKindResponse = "response"
)

// BodyRecord describes a raw HTTP body spool file.
type BodyRecord struct {
	File   string
	Format string
}

// BodySpool writes a single HTTP body to disk as bytes pass through the proxy.
type BodySpool struct {
	file *os.File

	mu        sync.Mutex
	closeOnce sync.Once
	bytes     int64
	closeErr  error
	writeErr  error
	// onClose releases the recorder's hold on this spool file; see
	// StreamSession.onClose.
	onClose func()
}

// BeginBody creates a raw body spool file.
func BeginBody(dir, clientID, kind string) (*BodyRecord, *BodySpool, error) {
	if dir == "" {
		return nil, nil, nil
	}
	if kind != BodyKindRequest && kind != BodyKindResponse {
		return nil, nil, fmt.Errorf("invalid body kind %q", kind)
	}
	bodyID := generateStreamID()
	relativePath := filepath.Join("bodies", safePath(clientID), fmt.Sprintf("%s-%s.bin", kind, bodyID))
	filePath := filepath.Join(dir, relativePath)
	if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create body spool dir: %w", err)
	}
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("create body spool: %w", err)
	}
	return &BodyRecord{
		File:   filepath.ToSlash(relativePath),
		Format: BodyFormatRaw,
	}, &BodySpool{file: file}, nil
}

// Write records body bytes to the spool file.
func (s *BodySpool) Write(p []byte) (int, error) {
	if s == nil || len(p) == 0 {
		return len(p), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	n, err := s.file.Write(p)
	s.bytes += int64(n)
	if err != nil {
		s.writeErr = err
	}
	return n, err
}

// Close closes the spool file. It is safe to call multiple times.
func (s *BodySpool) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closeErr = s.file.Close()
	})
	if s.onClose != nil {
		s.onClose()
	}
	return s.closeErr
}

// Bytes returns the number of bytes successfully written.
func (s *BodySpool) Bytes() int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// Err returns the first write or close error observed by the spool.
func (s *BodySpool) Err() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	return s.closeErr
}
