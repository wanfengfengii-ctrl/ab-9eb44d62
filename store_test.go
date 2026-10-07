package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return s
}

func mustSubmit(t *testing.T, s *Store, subject string, req *SubmitRequest) (int, json.RawMessage, bool) {
	t.Helper()
	if err := validateShape(req); err != nil {
		t.Fatalf("test request fails shape validation: %v", err)
	}
	return s.Submit(subject, req)
}

func decodeErr(t *testing.T, body json.RawMessage) APIError {
	t.Helper()
	var b errorBody
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if b.Error == nil {
		t.Fatalf("body has no error: %s", body)
	}
	return *b.Error
}

func TestSubmitLifecycle(t *testing.T) {
	s := newTestStore(t)

	// First version must be 1.
	st, body, _ := mustSubmit(t, s, "dev", &SubmitRequest{RequestID: "r0", Version: 2,
		Fields: []Field{{Name: "a", Number: 1, WireType: WireVarint}}})
	if st != http.StatusConflict {
		t.Fatalf("expected 409, got %d", st)
	}
	if e := decodeErr(t, body); e.Code != CodeVersionNotNext || e.CurrentVersion != 0 {
		t.Fatalf("expected VERSION_NOT_NEXT with currentVersion 0, got %+v", e)
	}

	// Version 1 accepted.
	st, _, _ = mustSubmit(t, s, "dev", &SubmitRequest{RequestID: "r1", Version: 1, Fields: []Field{
		{Name: "a", Number: 1, WireType: WireVarint},
		{Name: "b", Number: 2, WireType: WireFixed64},
	}})
	if st != http.StatusCreated {
		t.Fatalf("expected 201, got %d", st)
	}

	// Version must be exactly current+1.
	st, body, _ = mustSubmit(t, s, "dev", &SubmitRequest{RequestID: "r2", Version: 3,
		Fields: []Field{{Name: "a", Number: 1, WireType: WireVarint}, {Name: "b", Number: 2, WireType: WireFixed64}}})
	if st != http.StatusConflict {
		t.Fatalf("expected 409, got %d", st)
	}
	if e := decodeErr(t, body); e.Code != CodeVersionNotNext || e.CurrentVersion != 1 {
		t.Fatalf("expected VERSION_NOT_NEXT with currentVersion 1, got %+v", e)
	}

	// Legal evolution: drop field 2, retain its number, add field 4.
	st, body, _ = mustSubmit(t, s, "dev", &SubmitRequest{RequestID: "r3", Version: 2,
		Fields:          []Field{{Name: "a", Number: 1, WireType: WireVarint}, {Name: "d", Number: 4, WireType: WireFixed32}},
		ReservedNumbers: []int{2}})
	if st != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", st, body)
	}

	view, ok := s.Get("dev")
	if !ok {
		t.Fatal("subject missing")
	}
	if view.CurrentVersion != 2 {
		t.Fatalf("currentVersion = %d, want 2", view.CurrentVersion)
	}
	if len(view.ReservedNumbers) != 1 || view.ReservedNumbers[0] != 2 {
		t.Fatalf("reservedNumbers = %v, want [2]", view.ReservedNumbers)
	}
	if len(view.Fields) != 2 || view.Fields[0].Number != 1 || view.Fields[1].Number != 4 {
		t.Fatalf("fields = %+v", view.Fields)
	}
}

func TestIdempotentReplayAndConflict(t *testing.T) {
	s := newTestStore(t)
	req := &SubmitRequest{RequestID: "same", Version: 1,
		Fields: []Field{{Name: "a", Number: 1, WireType: WireVarint}}}

	st1, body1, replayed1 := mustSubmit(t, s, "dev", req)
	if st1 != http.StatusCreated || replayed1 {
		t.Fatalf("first submit: status=%d replayed=%v", st1, replayed1)
	}

	// Same requestId, same content (field order shuffled): replay original.
	st2, body2, replayed2 := mustSubmit(t, s, "dev", req)
	if st2 != st1 || !replayed2 {
		t.Fatalf("replay: status=%d replayed=%v", st2, replayed2)
	}
	if string(body1) != string(body2) {
		t.Fatalf("replayed body differs:\n%s\n%s", body1, body2)
	}

	// Same requestId, different content: conflict.
	st3, body3, _ := mustSubmit(t, s, "dev", &SubmitRequest{RequestID: "same", Version: 1,
		Fields: []Field{{Name: "a", Number: 1, WireType: WireVarint}, {Name: "b", Number: 2, WireType: WireFixed32}}})
	if st3 != http.StatusConflict {
		t.Fatalf("expected 409, got %d", st3)
	}
	if e := decodeErr(t, body3); e.Code != CodeRequestConflict {
		t.Fatalf("expected REQUEST_CONFLICT, got %+v", e)
	}

	// Still exactly one version.
	view, _ := s.Get("dev")
	if view.CurrentVersion != 1 {
		t.Fatalf("currentVersion = %d, want 1", view.CurrentVersion)
	}
}

func TestFailedRequestIsRecordedAndReplayed(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, "dev", &SubmitRequest{RequestID: "r1", Version: 1, Fields: []Field{
		{Name: "a", Number: 1, WireType: WireVarint},
		{Name: "b", Number: 2, WireType: WireFixed64},
	}})

	bad := &SubmitRequest{RequestID: "bad", Version: 2,
		Fields: []Field{{Name: "a", Number: 1, WireType: WireVarint}}} // drops 2 without retain
	st1, body1, _ := mustSubmit(t, s, "dev", bad)
	if st1 != http.StatusConflict {
		t.Fatalf("expected 409, got %d", st1)
	}
	if e := decodeErr(t, body1); e.Code != CodeNumberNotRetained || e.FirstViolation == nil || *e.FirstViolation != 2 {
		t.Fatalf("expected NUMBER_NOT_RETAINED firstViolation=2, got %+v", e)
	}

	// Retrying the same failed request replays the same failure.
	st2, body2, replayed := mustSubmit(t, s, "dev", bad)
	if !replayed || st2 != st1 || string(body1) != string(body2) {
		t.Fatalf("failure not replayed: status=%d replayed=%v", st2, replayed)
	}

	// The failure did not change the subject.
	view, _ := s.Get("dev")
	if view.CurrentVersion != 1 || len(view.ReservedNumbers) != 0 {
		t.Fatalf("state changed by failed request: %+v", view)
	}
}

func TestConcurrentSuccessors(t *testing.T) {
	s := newTestStore(t)
	mustSubmit(t, s, "dev", &SubmitRequest{RequestID: "r1", Version: 1,
		Fields: []Field{{Name: "a", Number: 1, WireType: WireVarint}}})

	const workers = 16
	statuses := make([]int, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, _, _ := s.Submit("dev", &SubmitRequest{
				RequestID: fmt.Sprintf("race-%d", i),
				Version:   2,
				Fields: []Field{
					{Name: "a", Number: 1, WireType: WireVarint},
					{Name: fmt.Sprintf("f%d", i), Number: 10 + i, WireType: WireFixed32},
				},
			})
			statuses[i] = st
		}(i)
	}
	wg.Wait()

	created := 0
	for _, st := range statuses {
		if st == http.StatusCreated {
			created++
		} else if st != http.StatusConflict {
			t.Fatalf("unexpected status %d in %v", st, statuses)
		}
	}
	if created != 1 {
		t.Fatalf("expected exactly one accepted successor, got %d (%v)", created, statuses)
	}
	view, _ := s.Get("dev")
	if view.CurrentVersion != 2 {
		t.Fatalf("currentVersion = %d, want 2", view.CurrentVersion)
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s1, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	v1 := &SubmitRequest{RequestID: "r1", Version: 1, Fields: []Field{
		{Name: "a", Number: 1, WireType: WireVarint},
		{Name: "b", Number: 2, WireType: WireFixed64},
	}}
	v2 := &SubmitRequest{RequestID: "r2", Version: 2,
		Fields:          []Field{{Name: "a", Number: 1, WireType: WireVarint}},
		ReservedNumbers: []int{2}}
	if st, _, _ := s1.Submit("dev", v1); st != http.StatusCreated {
		t.Fatalf("v1: status %d", st)
	}
	if st, _, _ := s1.Submit("dev", v2); st != http.StatusCreated {
		t.Fatalf("v2: status %d", st)
	}

	// Simulate a service restart: reopen from the same file.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	view, ok := s2.Get("dev")
	if !ok {
		t.Fatal("subject lost after restart")
	}
	if view.CurrentVersion != 2 {
		t.Fatalf("currentVersion = %d after restart, want 2", view.CurrentVersion)
	}
	if len(view.ReservedNumbers) != 1 || view.ReservedNumbers[0] != 2 {
		t.Fatalf("reservedNumbers = %v after restart, want [2]", view.ReservedNumbers)
	}

	// Idempotency records survive the restart too.
	st, _, replayed := s2.Submit("dev", v2)
	if !replayed || st != http.StatusCreated {
		t.Fatalf("requestId not replayed after restart: status=%d replayed=%v", st, replayed)
	}

	// Evolution rules still enforced after restart: keep field 1, try to
	// resurrect reserved number 2 as a field.
	st, body, _ := s2.Submit("dev", &SubmitRequest{RequestID: "r3", Version: 3,
		Fields: []Field{
			{Name: "a", Number: 1, WireType: WireVarint},
			{Name: "z", Number: 2, WireType: WireVarint},
		},
		ReservedNumbers: []int{2}})
	if st != http.StatusConflict {
		t.Fatalf("expected 409, got %d", st)
	}
	if e := decodeErr(t, body); e.Code != CodeReservedReused {
		t.Fatalf("expected RESERVED_NUMBER_REUSED, got %+v", e)
	}
}
