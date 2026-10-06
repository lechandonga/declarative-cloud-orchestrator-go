// Package state implements the versioned, lockable state file that records
// what the orchestrator believes exists in the cloud.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Version is the only state schema version this build understands.
const Version = 1

// ErrLocked is returned in non-blocking mode when another process holds the lock.
var ErrLocked = errors.New("state is locked by another process")

// VersionError reports an unreadable state schema version.
type VersionError struct {
	Found int
}

func (e *VersionError) Error() string {
	if e.Found > Version {
		return fmt.Sprintf("state version %d is newer than supported version %d; upgrade the tool", e.Found, Version)
	}
	return fmt.Sprintf("state version %d is too old (supported: %d); run migration instead of interpreting old data with new semantics", e.Found, Version)
}

// Resource records one managed resource.
type Resource struct {
	Type       string         `json:"type"`
	Name       string         `json:"name"`
	Properties map[string]any `json:"properties"`
}

// State is the persisted document.
type State struct {
	Version   int                  `json:"version"`
	Serial    int                  `json:"serial"`
	Resources map[string]*Resource `json:"resources"` // by address
}

// Store manages a state file with advisory locking.
type Store struct {
	path     string
	lockPath string
	lockFile *os.File
}

func NewStore(path string) *Store {
	return &Store{path: path, lockPath: path + ".lock"}
}

// Lock acquires the exclusive state lock. If wait is false it fails fast
// with ErrLocked; otherwise it blocks until the lock is available.
func (s *Store) Lock(wait bool) error {
	f, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if !wait {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			return ErrLocked
		}
	} else {
		// Poll with timeout-free blocking flock: orderly waiting.
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
			return err
		}
	}
	s.lockFile = f
	return nil
}

// Unlock releases the state lock.
func (s *Store) Unlock() error {
	if s.lockFile == nil {
		return nil
	}
	defer func() { s.lockFile = nil }()
	syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_UN)
	return s.lockFile.Close()
}

// Load reads and validates the state file. A missing file yields a fresh
// state; a file with an unknown version yields *VersionError.
func (s *Store) Load() (*State, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return &State{Version: Version, Resources: map[string]*Resource{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("corrupt state file %s: %w", s.path, err)
	}
	if st.Version != Version {
		return nil, &VersionError{Found: st.Version}
	}
	if st.Resources == nil {
		st.Resources = map[string]*Resource{}
	}
	return &st, nil
}

// Save persists the state atomically (temp file + rename), bumping Serial.
// Callers must hold the lock.
func (s *Store) Save(st *State) error {
	st.Serial++
	st.Version = Version
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// WaitForUnlock is a test helper: polls until the lock is free.
func (s *Store) WaitForUnlock(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := s.Lock(false); err == nil {
			s.Unlock()
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
