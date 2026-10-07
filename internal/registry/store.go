package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Store holds all durable state and persists it to a single JSON file.
// Writes are atomic (temp file + fsync + rename) and happen before the
// corresponding API response is returned, so accepted state survives
// process restarts.
type Store struct {
	path     string
	subjects map[string]*subject
}

type storeFile struct {
	Subjects map[string]*subject `json:"subjects"`
}

// OpenStore loads the store file, or starts empty when it does not exist.
// A corrupt file is a hard error: silently dropping accepted state would
// violate the durability contract.
func OpenStore(path string) (*Store, error) {
	st := &Store{path: path, subjects: map[string]*subject{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read store %s: %w", path, err)
	}
	if len(data) == 0 {
		return st, nil
	}
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("corrupt store file %s: %w", path, err)
	}
	if f.Subjects != nil {
		st.subjects = f.Subjects
	}
	return st, nil
}

// saveLocked serializes and atomically replaces the store file.
// Callers must hold the service lock.
func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(storeFile{Subjects: s.subjects}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal store: %w", err)
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open temp store: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write temp store: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync temp store: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close temp store: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace store: %w", err)
	}
	return nil
}
