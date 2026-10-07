package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// storedVersion is one accepted schema version of a subject.
type storedVersion struct {
	Version         int       `json:"version"`
	Fields          []Field   `json:"fields"`
	ReservedNumbers []int     `json:"reservedNumbers"`
	CreatedAt       time.Time `json:"createdAt"`
}

// storedRequest records the outcome of a requestId so retries replay the
// original result instead of being applied twice.
type storedRequest struct {
	Fingerprint string          `json:"fingerprint"`
	Status      int             `json:"status"`
	Body        json.RawMessage `json:"body"`
}

type subjectState struct {
	CurrentVersion int                      `json:"currentVersion"`
	Versions       []storedVersion          `json:"versions"`
	Reserved       map[int]bool             `json:"reserved"`
	Requests       map[string]storedRequest `json:"requests"`
}

type storeData struct {
	Subjects map[string]*subjectState `json:"subjects"`
}

// Store is a mutex-guarded, file-backed registry of all subjects. The single
// mutex serializes check-and-apply so concurrent submissions of the same
// successor version succeed exactly once.
type Store struct {
	mu   sync.Mutex
	path string
	data storeData
}

// OpenStore loads the registry from path (if present) and returns it ready
// for use. State accepted by Submit is persisted before the call returns, so
// a reopened store reflects everything that was ever acknowledged.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path}
	s.data.Subjects = map[string]*subjectState{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &s.data); err != nil {
				return nil, fmt.Errorf("corrupt data file %s: %w", path, err)
			}
		}
	case os.IsNotExist(err):
		// fresh start
	default:
		return nil, err
	}
	if s.data.Subjects == nil {
		s.data.Subjects = map[string]*subjectState{}
	}
	return s, nil
}

// versionCreated is the body returned for an accepted version.
type versionCreated struct {
	Subject         string    `json:"subject"`
	Version         int       `json:"version"`
	Fields          []Field   `json:"fields"`
	ReservedNumbers []int     `json:"reservedNumbers"`
	CreatedAt       time.Time `json:"createdAt"`
}

// SubjectView is the body returned by GET /api/schemas/{subject}.
type SubjectView struct {
	Subject         string  `json:"subject"`
	CurrentVersion  int     `json:"currentVersion"`
	Fields          []Field `json:"fields"`
	ReservedNumbers []int   `json:"reservedNumbers"`
}

// Get returns the current version and cumulative reserved numbers of a
// subject. The second result is false when the subject does not exist.
func (s *Store) Get(subject string) (SubjectView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.data.Subjects[subject]
	if sub == nil || len(sub.Versions) == 0 {
		return SubjectView{}, false
	}
	cur := sub.Versions[len(sub.Versions)-1]
	return SubjectView{
		Subject:         subject,
		CurrentVersion:  sub.CurrentVersion,
		Fields:          append([]Field{}, cur.Fields...),
		ReservedNumbers: sortedKeys(sub.Reserved),
	}, true
}

// Submit validates and, when legal, appends a new schema version to a
// subject. It returns the HTTP status and response body to send back and
// whether the response is a replay of an earlier requestId. Every well-formed
// outcome (accepted or rejected) is recorded under its requestId and persisted
// before returning; a request that fails validation never changes the stored
// schema or reserved numbers.
func (s *Store) Submit(subject string, req *SubmitRequest) (int, json.RawMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sub := s.data.Subjects[subject]
	current := 0
	if sub != nil {
		current = sub.CurrentVersion
	}

	// Idempotency: a seen requestId either replays its original result or
	// conflicts with it.
	if sub != nil {
		if rec, ok := sub.Requests[req.RequestID]; ok {
			if rec.Fingerprint == fingerprint(req) {
				return rec.Status, rec.Body, true
			}
			return http.StatusConflict, mustMarshal(errorBody{Error: &APIError{
				Code:           CodeRequestConflict,
				Message:        fmt.Sprintf("requestId %q was already used for subject %q with different content", req.RequestID, subject),
				CurrentVersion: current,
			}}), false
		}
	}

	// record persists the outcome of a rejected (but well-formed) request so
	// a later retry with the same requestId replays the same answer.
	record := func(apiErr *APIError) (int, json.RawMessage, bool) {
		apiErr.CurrentVersion = current
		body := mustMarshal(errorBody{Error: apiErr})
		next := copySubject(sub)
		next.Requests[req.RequestID] = storedRequest{Fingerprint: fingerprint(req), Status: http.StatusConflict, Body: body}
		if err := s.commitLocked(subject, next); err != nil {
			return s.internalError(current)
		}
		return http.StatusConflict, body, false
	}

	// Version sequencing: the first version must be 1, afterwards exactly
	// current+1.
	want := 1
	if sub != nil {
		want = current + 1
	}
	if req.Version != want {
		var msg string
		if sub == nil {
			msg = fmt.Sprintf("first version of subject %q must be 1, got %d", subject, req.Version)
		} else {
			msg = fmt.Sprintf("subject %q is at version %d; next version must be %d, got %d", subject, current, want, req.Version)
		}
		return record(&APIError{Code: CodeVersionNotNext, Message: msg})
	}

	// Evolution rules against the current state.
	if apiErr := validateEvolution(sub, req); apiErr != nil {
		return record(apiErr)
	}

	// Accept the new version.
	now := time.Now().UTC()
	next := copySubject(sub)
	sv := storedVersion{
		Version:         req.Version,
		Fields:          sortedFields(req.Fields),
		ReservedNumbers: sortedInts(req.ReservedNumbers),
		CreatedAt:       now,
	}
	next.Versions = append(next.Versions, sv)
	next.CurrentVersion = req.Version
	for _, n := range req.ReservedNumbers {
		next.Reserved[n] = true
	}
	body := mustMarshal(versionCreated{
		Subject:         subject,
		Version:         req.Version,
		Fields:          sv.Fields,
		ReservedNumbers: sortedKeys(next.Reserved),
		CreatedAt:       now,
	})
	next.Requests[req.RequestID] = storedRequest{Fingerprint: fingerprint(req), Status: http.StatusCreated, Body: body}
	if err := s.commitLocked(subject, next); err != nil {
		return s.internalError(current)
	}
	return http.StatusCreated, body, false
}

func (s *Store) internalError(current int) (int, json.RawMessage, bool) {
	return http.StatusInternalServerError, mustMarshal(errorBody{Error: &APIError{
		Code:           CodeInternal,
		Message:        "could not persist state; the request was not applied",
		CurrentVersion: current,
	}}), false
}

// commitLocked swaps in a new subject state and persists it. On persistence
// failure the previous in-memory state is restored, so a failed request never
// changes the subject.
func (s *Store) commitLocked(subject string, next *subjectState) error {
	prev, existed := s.data.Subjects[subject]
	s.data.Subjects[subject] = next
	if err := s.persistLocked(); err != nil {
		if existed {
			s.data.Subjects[subject] = prev
		} else {
			delete(s.data.Subjects, subject)
		}
		return err
	}
	return nil
}

// persistLocked writes the whole registry atomically: temp file, fsync,
// rename, then a best-effort directory fsync so the rename itself is durable.
func (s *Store) persistLocked() error {
	raw, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func copySubject(sub *subjectState) *subjectState {
	if sub == nil {
		return &subjectState{
			Reserved: map[int]bool{},
			Requests: map[string]storedRequest{},
		}
	}
	c := *sub
	c.Versions = append([]storedVersion{}, sub.Versions...)
	c.Reserved = maps.Clone(sub.Reserved)
	if c.Reserved == nil {
		c.Reserved = map[int]bool{}
	}
	c.Requests = maps.Clone(sub.Requests)
	if c.Requests == nil {
		c.Requests = map[string]storedRequest{}
	}
	return &c
}

func sortedKeys(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
