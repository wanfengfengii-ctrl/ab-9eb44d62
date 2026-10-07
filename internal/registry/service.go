package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Result is the outcome of a Publish call: a ready-to-send HTTP response.
type Result struct {
	Status int
	Body   []byte
	// Replay is true when the result was replayed from an idempotency
	// record instead of being freshly evaluated.
	Replay bool
}

// Service serializes all state transitions behind one mutex, which is
// what guarantees that concurrent publishes of the same successor
// version can succeed at most once.
type Service struct {
	mu    sync.Mutex
	store *Store
	now   func() time.Time
}

// NewService creates a Service on top of store.
func NewService(store *Store) *Service {
	return &Service{store: store, now: func() time.Time { return time.Now().UTC() }}
}

// Publish validates and (when legal) commits a new schema version.
//
// Order of evaluation:
//  1. idempotency replay / requestId conflict
//  2. request shape validation        -> 400 INVALID_REQUEST
//  3. version chain validation        -> 409 VERSION_CONFLICT
//  4. evolution rules                 -> 422 SCHEMA_EVOLUTION_VIOLATION
//  5. commit + persist                -> 201
//
// Every outcome of steps 2-5 is recorded under the requestId so retries
// replay the original result. Failures never mutate the current schema
// or the reserved-number tombstones.
func (s *Service) Publish(name string, req PublishRequest) Result {
	s.mu.Lock()
	defer s.mu.Unlock()

	if req.RequestID == "" {
		return errorResult(http.StatusBadRequest, ErrorBody{
			Code:    CodeInvalidRequest,
			Message: "requestId is required",
		})
	}

	sub := s.store.subjects[name]
	hash := contentHash(req)

	if sub != nil {
		if rec, ok := sub.Idempotency[req.RequestID]; ok {
			if rec.ContentHash == hash {
				return Result{Status: rec.Status, Body: rec.Body, Replay: true}
			}
			return errorResult(http.StatusConflict, ErrorBody{
				Code:           CodeRequestIDConflict,
				Message:        fmt.Sprintf("requestId %q was already used for subject %q with different content", req.RequestID, name),
				CurrentVersion: intPtr(sub.CurrentVersion),
			})
		}
	}

	if err := validateShape(req); err != nil {
		return s.record(name, sub, req.RequestID, hash, http.StatusBadRequest, ErrorResponse{Error: ErrorBody{
			Code:    CodeInvalidRequest,
			Message: err.Error(),
		}})
	}

	current := 0
	if sub != nil {
		current = sub.CurrentVersion
	}

	if *req.Version != current+1 {
		return s.record(name, sub, req.RequestID, hash, http.StatusConflict, ErrorResponse{Error: ErrorBody{
			Code:           CodeVersionConflict,
			Message:        fmt.Sprintf("version %d does not succeed current version %d", *req.Version, current),
			CurrentVersion: intPtr(current),
		}})
	}

	if violations := validateEvolution(sub, *req.Fields); len(violations) > 0 {
		first := violations[0].Number
		return s.record(name, sub, req.RequestID, hash, http.StatusUnprocessableEntity, ErrorResponse{Error: ErrorBody{
			Code:            CodeEvolutionViolation,
			Message:         fmt.Sprintf("illegal schema evolution: first violation at field number %d (%s)", first, violations[0].Rule),
			CurrentVersion:  intPtr(current),
			ViolatingNumber: intPtr(first),
			Violations:      violations,
		}})
	}

	// Commit: build the next subject state, persist it, then swap it in.
	fields := sortedFields(*req.Fields)
	now := s.now()
	ver := Version{Version: *req.Version, Fields: fields, RequestID: req.RequestID, CreatedAt: now}

	next := &subject{}
	if sub != nil {
		next = cloneSubject(sub)
	}
	if next.Idempotency == nil {
		next.Idempotency = map[string]idemRecord{}
	}
	next.CurrentVersion = ver.Version
	next.Versions = append(next.Versions, ver)

	// Tombstone every active number that disappeared from the new schema.
	// Reserved numbers are cumulative: they are never revoked or reused.
	active := make(map[int]bool, len(fields))
	for _, f := range fields {
		active[f.Number] = true
	}
	reserved := next.reservedSet()
	if sub != nil {
		for _, f := range sub.currentFields() {
			if !active[f.Number] {
				reserved[f.Number] = true
			}
		}
	}
	next.Reserved = sortedInts(reserved)

	resp := PublishResponse{
		Subject:         name,
		Version:         ver.Version,
		RequestID:       req.RequestID,
		Fields:          fields,
		ReservedNumbers: next.Reserved,
		CreatedAt:       now,
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return internalError(err)
	}
	next.Idempotency[req.RequestID] = idemRecord{ContentHash: hash, Status: http.StatusCreated, Body: body}

	prev := s.store.subjects[name]
	s.store.subjects[name] = next
	if err := s.store.saveLocked(); err != nil {
		restoreSubject(s.store, name, prev)
		return internalError(err)
	}
	return Result{Status: http.StatusCreated, Body: body}
}

// Get returns the current schema view of a subject. The second return
// value is false when the subject has no accepted version.
func (s *Service) Get(name string) (SchemaResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub := s.store.subjects[name]
	if sub == nil || len(sub.Versions) == 0 {
		return SchemaResponse{}, false
	}
	versions := make([]int, len(sub.Versions))
	for i, v := range sub.Versions {
		versions[i] = v.Version
	}
	return SchemaResponse{
		Subject:         name,
		CurrentVersion:  sub.CurrentVersion,
		Fields:          append([]Field{}, sub.currentFields()...),
		ReservedNumbers: append([]int{}, sub.Reserved...),
		Versions:        versions,
	}, true
}

// record persists an idempotency record for a terminal (non-committed)
// outcome and returns the corresponding result.
func (s *Service) record(name string, sub *subject, requestID, hash string, status int, payload ErrorResponse) Result {
	body, err := json.Marshal(payload)
	if err != nil {
		return internalError(err)
	}
	work := sub
	if work == nil {
		work = &subject{}
	} else {
		work = cloneSubject(work)
	}
	if work.Idempotency == nil {
		work.Idempotency = map[string]idemRecord{}
	}
	work.Idempotency[requestID] = idemRecord{ContentHash: hash, Status: status, Body: body}
	prev := s.store.subjects[name]
	s.store.subjects[name] = work
	if err := s.store.saveLocked(); err != nil {
		restoreSubject(s.store, name, prev)
		return internalError(err)
	}
	return Result{Status: status, Body: body}
}

func restoreSubject(store *Store, name string, prev *subject) {
	if prev == nil {
		delete(store.subjects, name)
		return
	}
	store.subjects[name] = prev
}

func errorResult(status int, body ErrorBody) Result {
	data, err := json.Marshal(ErrorResponse{Error: body})
	if err != nil {
		return internalError(err)
	}
	return Result{Status: status, Body: data}
}

func internalError(err error) Result {
	data, _ := json.Marshal(ErrorResponse{Error: ErrorBody{
		Code:    CodeInternal,
		Message: err.Error(),
	}})
	return Result{Status: http.StatusInternalServerError, Body: data}
}

// validateShape checks the request payload itself, independent of any
// existing subject state.
func validateShape(req PublishRequest) error {
	if len(req.RequestID) > 256 {
		return errors.New("requestId must be at most 256 characters")
	}
	if req.Version == nil {
		return errors.New("version is required")
	}
	if *req.Version < 1 {
		return fmt.Errorf("version must be >= 1, got %d", *req.Version)
	}
	if req.Fields == nil {
		return errors.New("fields is required (use [] for an empty schema)")
	}
	seenNumbers := map[int]string{}
	seenNames := map[string]int{}
	for i, f := range *req.Fields {
		if f.Name == "" {
			return fmt.Errorf("fields[%d]: name is required", i)
		}
		if len(f.Name) > 128 {
			return fmt.Errorf("fields[%d]: name must be at most 128 characters", i)
		}
		if f.Number < 1 {
			return fmt.Errorf("fields[%d] (%q): number must be >= 1", i, f.Name)
		}
		if !f.WireType.Valid() {
			return fmt.Errorf("fields[%d] (%q): wireType %q is not one of VARINT, FIXED32, FIXED64, LENGTH_DELIMITED", i, f.Name, f.WireType)
		}
		if prev, dup := seenNumbers[f.Number]; dup {
			return fmt.Errorf("duplicate field number %d (%q and %q)", f.Number, prev, f.Name)
		}
		seenNumbers[f.Number] = f.Name
		if prev, dup := seenNames[f.Name]; dup {
			return fmt.Errorf("duplicate field name %q (numbers %d and %d)", f.Name, prev, f.Number)
		}
		seenNames[f.Name] = f.Number
	}
	return nil
}

// validateEvolution checks the new field list against the current schema
// and the cumulative reserved-number tombstones. Violations are sorted by
// field number so the first one is the smallest violating number.
func validateEvolution(sub *subject, fields []Field) []Violation {
	if sub == nil || len(sub.Versions) == 0 {
		return nil
	}
	active := map[int]Field{}
	for _, f := range sub.currentFields() {
		active[f.Number] = f
	}
	reserved := sub.reservedSet()
	var out []Violation
	for _, f := range fields {
		if a, ok := active[f.Number]; ok {
			if a.Name != f.Name {
				out = append(out, Violation{
					Number:  f.Number,
					Rule:    RuleFieldRenamed,
					Message: fmt.Sprintf("field number %d is named %q in version %d and cannot be renamed to %q", f.Number, a.Name, sub.CurrentVersion, f.Name),
				})
			}
			if a.WireType != f.WireType {
				out = append(out, Violation{
					Number:  f.Number,
					Rule:    RuleWireTypeChanged,
					Message: fmt.Sprintf("field number %d has wire type %s in version %d and cannot change to %s", f.Number, a.WireType, sub.CurrentVersion, f.WireType),
				})
			}
			continue
		}
		if reserved[f.Number] {
			out = append(out, Violation{
				Number:  f.Number,
				Rule:    RuleNumberReserved,
				Message: fmt.Sprintf("field number %d is reserved by a previously deleted field and cannot be reused", f.Number),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Number != out[j].Number {
			return out[i].Number < out[j].Number
		}
		return out[i].Rule < out[j].Rule
	})
	return out
}
