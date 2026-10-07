package registry

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
)

func newService(t *testing.T) (*Service, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return NewService(store), path
}

func fields(fs ...Field) *[]Field { return &fs }

func intp(v int) *int { return &v }

func publish(t *testing.T, svc *Service, subject, reqID string, version int, fs ...Field) Result {
	t.Helper()
	return svc.Publish(subject, PublishRequest{RequestID: reqID, Version: intp(version), Fields: fields(fs...)})
}

func decodeError(t *testing.T, res Result) ErrorBody {
	t.Helper()
	var resp ErrorResponse
	if err := json.Unmarshal(res.Body, &resp); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	return resp.Error
}

func TestFirstVersionMustBeOne(t *testing.T) {
	svc, _ := newService(t)

	res := publish(t, svc, "s", "r1", 2, Field{Name: "a", Number: 1, WireType: WireVarint})
	if res.Status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.Status)
	}
	body := decodeError(t, res)
	if body.Code != CodeVersionConflict {
		t.Fatalf("code = %q, want VERSION_CONFLICT", body.Code)
	}
	if body.CurrentVersion == nil || *body.CurrentVersion != 0 {
		t.Fatalf("currentVersion = %v, want 0", body.CurrentVersion)
	}

	res = publish(t, svc, "s", "r2", 1, Field{Name: "a", Number: 1, WireType: WireVarint})
	if res.Status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", res.Status)
	}

	res = publish(t, svc, "s", "r3", 3, Field{Name: "a", Number: 1, WireType: WireVarint})
	if res.Status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for gap", res.Status)
	}
	if body := decodeError(t, res); body.Code != CodeVersionConflict {
		t.Fatalf("code = %q, want VERSION_CONFLICT", body.Code)
	}
}

func TestRenameAndRetypeRejected(t *testing.T) {
	svc, _ := newService(t)
	publish(t, svc, "s", "r1", 1,
		Field{Name: "alpha", Number: 1, WireType: WireVarint},
		Field{Name: "beta", Number: 2, WireType: WireFixed32},
	)

	res := publish(t, svc, "s", "r2", 2,
		Field{Name: "alphaRenamed", Number: 1, WireType: WireVarint},
		Field{Name: "beta", Number: 2, WireType: WireFixed32},
	)
	if res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("rename status = %d, want 422", res.Status)
	}
	body := decodeError(t, res)
	if body.Code != CodeEvolutionViolation {
		t.Fatalf("code = %q, want SCHEMA_EVOLUTION_VIOLATION", body.Code)
	}
	if body.CurrentVersion == nil || *body.CurrentVersion != 1 {
		t.Fatalf("currentVersion = %v, want 1", body.CurrentVersion)
	}
	if body.ViolatingNumber == nil || *body.ViolatingNumber != 1 {
		t.Fatalf("violatingNumber = %v, want 1", body.ViolatingNumber)
	}

	res = publish(t, svc, "s", "r3", 2,
		Field{Name: "alpha", Number: 1, WireType: WireFixed64},
		Field{Name: "beta", Number: 2, WireType: WireFixed32},
	)
	if res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("retype status = %d, want 422", res.Status)
	}
	if body := decodeError(t, res); body.Code != CodeEvolutionViolation {
		t.Fatalf("code = %q, want SCHEMA_EVOLUTION_VIOLATION", body.Code)
	}
}

func TestViolatingNumberIsSmallest(t *testing.T) {
	svc, _ := newService(t)
	publish(t, svc, "s", "r1", 1,
		Field{Name: "a", Number: 5, WireType: WireVarint},
		Field{Name: "b", Number: 9, WireType: WireVarint},
		Field{Name: "c", Number: 12, WireType: WireVarint},
	)
	res := publish(t, svc, "s", "r2", 2,
		Field{Name: "a2", Number: 5, WireType: WireVarint},
		Field{Name: "b2", Number: 9, WireType: WireVarint},
		Field{Name: "c2", Number: 12, WireType: WireVarint},
	)
	body := decodeError(t, res)
	if body.ViolatingNumber == nil || *body.ViolatingNumber != 5 {
		t.Fatalf("violatingNumber = %v, want 5 (smallest)", body.ViolatingNumber)
	}
	if len(body.Violations) != 3 {
		t.Fatalf("violations = %d, want 3", len(body.Violations))
	}
}

func TestDeletionTombstonesNumber(t *testing.T) {
	svc, _ := newService(t)
	publish(t, svc, "s", "r1", 1,
		Field{Name: "a", Number: 1, WireType: WireVarint},
		Field{Name: "b", Number: 2, WireType: WireFixed32},
	)
	res := publish(t, svc, "s", "r2", 2, Field{Name: "a", Number: 1, WireType: WireVarint})
	if res.Status != http.StatusCreated {
		t.Fatalf("v2 status = %d, want 201", res.Status)
	}

	schema, ok := svc.Get("s")
	if !ok {
		t.Fatal("subject not found")
	}
	if len(schema.ReservedNumbers) != 1 || schema.ReservedNumbers[0] != 2 {
		t.Fatalf("reservedNumbers = %v, want [2]", schema.ReservedNumbers)
	}

	// Reusing the tombstoned number for a new field is rejected.
	res = publish(t, svc, "s", "r3", 3,
		Field{Name: "a", Number: 1, WireType: WireVarint},
		Field{Name: "z", Number: 2, WireType: WireLengthDelimited},
	)
	if res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("reuse status = %d, want 422", res.Status)
	}
	if body := decodeError(t, res); body.ViolatingNumber == nil || *body.ViolatingNumber != 2 {
		t.Fatalf("violatingNumber = %v, want 2", body.ViolatingNumber)
	}

	// Reviving the deleted field with identical name/type is also rejected.
	res = publish(t, svc, "s", "r4", 3,
		Field{Name: "a", Number: 1, WireType: WireVarint},
		Field{Name: "b", Number: 2, WireType: WireFixed32},
	)
	if res.Status != http.StatusUnprocessableEntity {
		t.Fatalf("revive status = %d, want 422", res.Status)
	}
}

func TestTombstonesAreCumulative(t *testing.T) {
	svc, _ := newService(t)
	publish(t, svc, "s", "r1", 1,
		Field{Name: "a", Number: 1, WireType: WireVarint},
		Field{Name: "b", Number: 2, WireType: WireVarint},
		Field{Name: "c", Number: 3, WireType: WireVarint},
	)
	publish(t, svc, "s", "r2", 2,
		Field{Name: "a", Number: 1, WireType: WireVarint},
		Field{Name: "c", Number: 3, WireType: WireVarint},
	)
	publish(t, svc, "s", "r3", 3,
		Field{Name: "a", Number: 1, WireType: WireVarint},
	)
	schema, _ := svc.Get("s")
	if len(schema.ReservedNumbers) != 2 || schema.ReservedNumbers[0] != 2 || schema.ReservedNumbers[1] != 3 {
		t.Fatalf("reservedNumbers = %v, want [2 3]", schema.ReservedNumbers)
	}
}

func TestFailedRequestDoesNotMutateState(t *testing.T) {
	svc, _ := newService(t)
	publish(t, svc, "s", "r1", 1,
		Field{Name: "a", Number: 1, WireType: WireVarint},
		Field{Name: "b", Number: 2, WireType: WireFixed32},
	)
	publish(t, svc, "s", "r2", 2, Field{Name: "a", Number: 1, WireType: WireVarint})
	before, _ := svc.Get("s")

	// A batch of failing requests of every kind.
	publish(t, svc, "s", "f1", 5, Field{Name: "a", Number: 1, WireType: WireVarint})          // version gap
	publish(t, svc, "s", "f2", 3, Field{Name: "renamed", Number: 1, WireType: WireVarint})    // rename
	publish(t, svc, "s", "f3", 3, Field{Name: "x", Number: 2, WireType: WireVarint})          // reserved reuse
	publish(t, svc, "s", "f4", 3, Field{Name: "a", Number: 1, WireType: "BOGUS"})             // bad wire type
	publish(t, svc, "s", "f5", 3, Field{Name: "a", Number: 1, WireType: WireLengthDelimited}) // retype

	after, ok := svc.Get("s")
	if !ok {
		t.Fatal("subject lost after failed requests")
	}
	if after.CurrentVersion != before.CurrentVersion {
		t.Fatalf("currentVersion changed: %d -> %d", before.CurrentVersion, after.CurrentVersion)
	}
	if len(after.ReservedNumbers) != len(before.ReservedNumbers) {
		t.Fatalf("reservedNumbers changed: %v -> %v", before.ReservedNumbers, after.ReservedNumbers)
	}
	for i := range after.ReservedNumbers {
		if after.ReservedNumbers[i] != before.ReservedNumbers[i] {
			t.Fatalf("reservedNumbers changed: %v -> %v", before.ReservedNumbers, after.ReservedNumbers)
		}
	}
	if len(after.Fields) != len(before.Fields) {
		t.Fatalf("fields changed: %v -> %v", before.Fields, after.Fields)
	}
	if len(after.Versions) != len(before.Versions) {
		t.Fatalf("versions changed: %v -> %v", before.Versions, after.Versions)
	}
}

func TestIdempotentReplay(t *testing.T) {
	svc, _ := newService(t)
	req := PublishRequest{
		RequestID: "same-id",
		Version:   intp(1),
		Fields:    fields(Field{Name: "a", Number: 1, WireType: WireVarint}),
	}
	first := svc.Publish("s", req)
	if first.Status != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.Status)
	}
	second := svc.Publish("s", req)
	if second.Status != http.StatusCreated || !second.Replay {
		t.Fatalf("second = (%d, replay=%v), want (201, true)", second.Status, second.Replay)
	}
	if string(first.Body) != string(second.Body) {
		t.Fatalf("replay body differs:\n%s\n%s", first.Body, second.Body)
	}
	schema, _ := svc.Get("s")
	if schema.CurrentVersion != 1 || len(schema.Versions) != 1 {
		t.Fatalf("replay mutated state: %+v", schema)
	}

	// Field order in the payload does not affect identity.
	reordered := PublishRequest{
		RequestID: "order-id",
		Version:   intp(1),
		Fields: fields(
			Field{Name: "b", Number: 2, WireType: WireFixed32},
			Field{Name: "a", Number: 1, WireType: WireVarint},
		),
	}
	a := svc.Publish("s2", reordered)
	reordered.Fields = fields(
		Field{Name: "a", Number: 1, WireType: WireVarint},
		Field{Name: "b", Number: 2, WireType: WireFixed32},
	)
	b := svc.Publish("s2", reordered)
	if !b.Replay || string(a.Body) != string(b.Body) {
		t.Fatalf("reordered retry was not replayed")
	}
}

func TestRequestIDConflict(t *testing.T) {
	svc, _ := newService(t)
	publish(t, svc, "s", "rid", 1, Field{Name: "a", Number: 1, WireType: WireVarint})
	res := publish(t, svc, "s", "rid", 1, Field{Name: "different", Number: 1, WireType: WireVarint})
	if res.Status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.Status)
	}
	if body := decodeError(t, res); body.Code != CodeRequestIDConflict {
		t.Fatalf("code = %q, want REQUEST_ID_CONFLICT", body.Code)
	}

	// The same requestId on a different subject is independent.
	res = publish(t, svc, "other", "rid", 1, Field{Name: "different", Number: 1, WireType: WireVarint})
	if res.Status != http.StatusCreated {
		t.Fatalf("cross-subject reuse status = %d, want 201", res.Status)
	}
}

func TestFailureOutcomeIsRecordedAndReplayed(t *testing.T) {
	svc, _ := newService(t)
	req := PublishRequest{
		RequestID: "fail-id",
		Version:   intp(7),
		Fields:    fields(Field{Name: "a", Number: 1, WireType: WireVarint}),
	}
	first := svc.Publish("s", req)
	if first.Status != http.StatusConflict {
		t.Fatalf("first status = %d, want 409", first.Status)
	}
	second := svc.Publish("s", req)
	if second.Status != http.StatusConflict || !second.Replay {
		t.Fatalf("second = (%d, replay=%v), want (409, true)", second.Status, second.Replay)
	}
	if string(first.Body) != string(second.Body) {
		t.Fatalf("replayed failure body differs")
	}
}

func TestConcurrentSuccessorsSingleWinner(t *testing.T) {
	svc, _ := newService(t)
	publish(t, svc, "s", "r0", 1, Field{Name: "a", Number: 1, WireType: WireVarint})

	const racers = 16
	results := make(chan Result, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			reqID := "race-" + string(rune('a'+i))
			results <- svc.Publish("s", PublishRequest{
				RequestID: reqID,
				Version:   intp(2),
				Fields: fields(
					Field{Name: "a", Number: 1, WireType: WireVarint},
					Field{Name: "extra", Number: 10 + i, WireType: WireFixed32},
				),
			})
		}(i)
	}
	wg.Wait()
	close(results)

	created, conflicts := 0, 0
	for res := range results {
		switch res.Status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status %d", res.Status)
		}
	}
	if created != 1 || conflicts != racers-1 {
		t.Fatalf("created=%d conflicts=%d, want 1 and %d", created, conflicts, racers-1)
	}
	schema, _ := svc.Get("s")
	if schema.CurrentVersion != 2 || len(schema.Versions) != 2 {
		t.Fatalf("state after race: %+v", schema)
	}
}

func TestConcurrentIdenticalRequestsSingleCommit(t *testing.T) {
	svc, _ := newService(t)
	req := PublishRequest{
		RequestID: "same",
		Version:   intp(1),
		Fields:    fields(Field{Name: "a", Number: 1, WireType: WireVarint}),
	}
	const racers = 16
	results := make(chan Result, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- svc.Publish("s", req)
		}()
	}
	wg.Wait()
	close(results)

	var firstBody []byte
	for res := range results {
		if res.Status != http.StatusCreated {
			t.Fatalf("status = %d, want 201 for all", res.Status)
		}
		if firstBody == nil {
			firstBody = res.Body
		} else if string(firstBody) != string(res.Body) {
			t.Fatal("bodies diverged")
		}
	}
	schema, _ := svc.Get("s")
	if schema.CurrentVersion != 1 || len(schema.Versions) != 1 {
		t.Fatalf("state after identical race: %+v", schema)
	}
}

func TestStateSurvivesReload(t *testing.T) {
	svc, path := newService(t)
	publish(t, svc, "s", "r1", 1,
		Field{Name: "a", Number: 1, WireType: WireVarint},
		Field{Name: "b", Number: 2, WireType: WireFixed32},
	)
	publish(t, svc, "s", "r2", 2, Field{Name: "a", Number: 1, WireType: WireVarint})

	// Simulate a restart: reopen the store from disk.
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	restarted := NewService(store)

	schema, ok := restarted.Get("s")
	if !ok {
		t.Fatal("subject missing after restart")
	}
	if schema.CurrentVersion != 2 {
		t.Fatalf("currentVersion = %d, want 2", schema.CurrentVersion)
	}
	if len(schema.ReservedNumbers) != 1 || schema.ReservedNumbers[0] != 2 {
		t.Fatalf("reservedNumbers = %v, want [2]", schema.ReservedNumbers)
	}
	if len(schema.Fields) != 1 || schema.Fields[0].Name != "a" {
		t.Fatalf("fields = %+v, want [a]", schema.Fields)
	}

	// Idempotency records survive too: the v2 request replays.
	res := restarted.Publish("s", PublishRequest{
		RequestID: "r2",
		Version:   intp(2),
		Fields:    fields(Field{Name: "a", Number: 1, WireType: WireVarint}),
	})
	if res.Status != http.StatusCreated || !res.Replay {
		t.Fatalf("replay after restart = (%d, %v), want (201, true)", res.Status, res.Replay)
	}

	// And the version chain continues from the reloaded state.
	res = restarted.Publish("s", PublishRequest{
		RequestID: "r3",
		Version:   intp(3),
		Fields:    fields(Field{Name: "a", Number: 1, WireType: WireVarint}, Field{Name: "c", Number: 5, WireType: WireFixed64}),
	})
	if res.Status != http.StatusCreated {
		t.Fatalf("v3 after restart = %d, want 201", res.Status)
	}
}

func TestGetUnknownSubject(t *testing.T) {
	svc, _ := newService(t)
	if _, ok := svc.Get("nope"); ok {
		t.Fatal("unknown subject reported as found")
	}
	// A subject with only failed publishes is still unknown.
	publish(t, svc, "bad", "r1", 9, Field{Name: "a", Number: 1, WireType: WireVarint})
	if _, ok := svc.Get("bad"); ok {
		t.Fatal("subject with only failures reported as found")
	}
}

func TestShapeValidation(t *testing.T) {
	svc, _ := newService(t)
	cases := []struct {
		name string
		req  PublishRequest
	}{
		{"missing version", PublishRequest{RequestID: "a", Fields: fields()}},
		{"zero version", PublishRequest{RequestID: "a", Version: intp(0), Fields: fields()}},
		{"missing fields", PublishRequest{RequestID: "a", Version: intp(1)}},
		{"empty name", PublishRequest{RequestID: "a", Version: intp(1), Fields: fields(Field{Name: "", Number: 1, WireType: WireVarint})}},
		{"zero number", PublishRequest{RequestID: "a", Version: intp(1), Fields: fields(Field{Name: "a", Number: 0, WireType: WireVarint})}},
		{"bad wire type", PublishRequest{RequestID: "a", Version: intp(1), Fields: fields(Field{Name: "a", Number: 1, WireType: "STRING"})}},
		{"dup number", PublishRequest{RequestID: "a", Version: intp(1), Fields: fields(
			Field{Name: "a", Number: 1, WireType: WireVarint},
			Field{Name: "b", Number: 1, WireType: WireFixed32},
		)}},
		{"dup name", PublishRequest{RequestID: "a", Version: intp(1), Fields: fields(
			Field{Name: "a", Number: 1, WireType: WireVarint},
			Field{Name: "a", Number: 2, WireType: WireFixed32},
		)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := svc.Publish("shape-"+tc.name, tc.req)
			if res.Status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", res.Status)
			}
			if body := decodeError(t, res); body.Code != CodeInvalidRequest {
				t.Fatalf("code = %q, want INVALID_REQUEST", body.Code)
			}
		})
	}
}

func TestEmptySchemaDeletesEverything(t *testing.T) {
	svc, _ := newService(t)
	publish(t, svc, "s", "r1", 1,
		Field{Name: "a", Number: 1, WireType: WireVarint},
		Field{Name: "b", Number: 7, WireType: WireFixed64},
	)
	res := publish(t, svc, "s", "r2", 2)
	if res.Status != http.StatusCreated {
		t.Fatalf("empty schema status = %d, want 201", res.Status)
	}
	schema, _ := svc.Get("s")
	if len(schema.Fields) != 0 {
		t.Fatalf("fields = %+v, want empty", schema.Fields)
	}
	if len(schema.ReservedNumbers) != 2 {
		t.Fatalf("reservedNumbers = %v, want [1 7]", schema.ReservedNumbers)
	}
}
