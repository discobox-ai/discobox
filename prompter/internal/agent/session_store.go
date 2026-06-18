package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

var ErrEmptySessionID = errors.New("empty session id")

// SessionRecord maps a caller-owned prompter session ID to an agent-owned
// provider session identifier. Adapters should create records only for
// persistent runs; an empty RunRequest.SessionID is ephemeral and should not be
// written here.
type SessionRecord struct {
	Agent             Kind      `json:"agent"`
	Workdir           string    `json:"workdir"`
	CallerSessionID   string    `json:"callerSessionID"`
	ProviderSessionID string    `json:"providerSessionID"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// SessionStore persists session mappings for adapters that cannot use the
// caller-provided session ID directly.
type SessionStore struct {
	Path string
}

// DefaultSessionStore returns the default on-disk session store.
func DefaultSessionStore() (SessionStore, error) {
	path, err := DefaultSessionStorePath()
	if err != nil {
		return SessionStore{}, err
	}
	return SessionStore{Path: path}, nil
}

// DefaultSessionStorePath resolves the state file used for persistent session
// mappings.
func DefaultSessionStorePath() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "discobox", "prompter", "sessions.json"), nil
}

// Get returns the mapping for one caller session in one workdir and agent.
func (s SessionStore) Get(agent Kind, workdir string, callerSessionID string) (SessionRecord, bool, error) {
	if callerSessionID == "" {
		return SessionRecord{}, false, ErrEmptySessionID
	}
	workdir, err := normalizeSessionWorkdir(workdir)
	if err != nil {
		return SessionRecord{}, false, err
	}

	var found SessionRecord
	ok := false
	err = s.withLock(func() error {
		file, err := s.read()
		if err != nil {
			return err
		}
		for _, record := range file.Sessions {
			if record.Agent == agent && record.Workdir == workdir && record.CallerSessionID == callerSessionID {
				found = record
				ok = true
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return SessionRecord{}, false, err
	}
	return found, ok, nil
}

// Put creates or replaces one session mapping.
func (s SessionStore) Put(record SessionRecord) error {
	if record.CallerSessionID == "" {
		return ErrEmptySessionID
	}
	if record.ProviderSessionID == "" {
		return errors.New("empty provider session id")
	}
	workdir, err := normalizeSessionWorkdir(record.Workdir)
	if err != nil {
		return err
	}
	record.Workdir = workdir
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = time.Now().UTC()
	}

	return s.withLock(func() error {
		file, err := s.read()
		if err != nil {
			return err
		}
		for i, existing := range file.Sessions {
			if existing.Agent == record.Agent && existing.Workdir == record.Workdir && existing.CallerSessionID == record.CallerSessionID {
				file.Sessions[i] = record
				return s.write(file)
			}
		}
		file.Sessions = append(file.Sessions, record)
		return s.write(file)
	})
}

// Delete removes one persistent session mapping. It does not delete the
// provider-owned session itself.
func (s SessionStore) Delete(agent Kind, workdir string, callerSessionID string) error {
	if callerSessionID == "" {
		return ErrEmptySessionID
	}
	workdir, err := normalizeSessionWorkdir(workdir)
	if err != nil {
		return err
	}
	return s.withLock(func() error {
		file, err := s.read()
		if err != nil {
			return err
		}
		filtered := file.Sessions[:0]
		for _, record := range file.Sessions {
			if record.Agent == agent && record.Workdir == workdir && record.CallerSessionID == callerSessionID {
				continue
			}
			filtered = append(filtered, record)
		}
		file.Sessions = filtered
		return s.write(file)
	})
}

type sessionStoreFile struct {
	Version  int             `json:"version"`
	Sessions []SessionRecord `json:"sessions"`
}

func (s SessionStore) withLock(fn func() error) error {
	if s.Path == "" {
		return errors.New("empty session store path")
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return fmt.Errorf("create session store directory: %w", err)
	}
	lockPath := s.Path + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open session store lock: %w", err)
	}
	defer file.Close()
	if err := lockSessionStoreFile(file); err != nil {
		return fmt.Errorf("lock session store: %w", err)
	}
	defer func() {
		_ = unlockSessionStoreFile(file)
	}()
	return fn()
}

func (s SessionStore) read() (sessionStoreFile, error) {
	if s.Path == "" {
		return sessionStoreFile{}, errors.New("empty session store path")
	}
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return sessionStoreFile{Version: 1}, nil
	}
	if err != nil {
		return sessionStoreFile{}, fmt.Errorf("read session store: %w", err)
	}
	var file sessionStoreFile
	if err := json.Unmarshal(data, &file); err != nil {
		return sessionStoreFile{}, fmt.Errorf("parse session store: %w", err)
	}
	if file.Version == 0 {
		file.Version = 1
	}
	return file, nil
}

func (s SessionStore) write(file sessionStoreFile) error {
	if s.Path == "" {
		return errors.New("empty session store path")
	}
	file.Version = 1
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return fmt.Errorf("create session store directory: %w", err)
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session store: %w", err)
	}
	data = append(data, '\n')
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write session store: %w", err)
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		return fmt.Errorf("replace session store: %w", err)
	}
	return nil
}

func normalizeSessionWorkdir(workdir string) (string, error) {
	if workdir == "" {
		return "", errors.New("empty workdir")
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return "", fmt.Errorf("resolve session workdir: %w", err)
	}
	return filepath.Clean(abs), nil
}
