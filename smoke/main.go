// Command smoke runs an end-to-end smoke suite against a live schema
// registry API: publish, evolve, idempotent replay, conflict handling,
// concurrent successor races and reserved-number tombstone rules. It exits
// non-zero if any check fails, and uses a unique subject per run so it can be
// re-run against the same service any number of times.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"
)

var (
	addr = flag.String("addr", "http://localhost:8080", "base URL of the schema registry API")
	wait = flag.Duration("wait", 60*time.Second, "how long to wait for the API to become healthy")
)

var failures int

func check(name string, ok bool, detail ...any) {
	if ok {
		fmt.Printf("PASS %s\n", name)
		return
	}
	failures++
	fmt.Printf("FAIL %s: %s\n", name, strings.TrimSpace(fmt.Sprintln(detail...)))
}

type response struct {
	status int
	header http.Header
	body   map[string]any
	raw    []byte
}

func (r *response) errCode() string {
	e, _ := r.body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func (r *response) errField(field string) any {
	e, _ := r.body["error"].(map[string]any)
	return e[field]
}

var hc = &http.Client{Timeout: 10 * time.Second}

func call(method, url string, payload any) (*response, error) {
	var rdr io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	out := &response{status: resp.StatusCode, header: resp.Header, raw: raw}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out.body)
	}
	return out, nil
}

func post(subject string, payload any) (*response, error) {
	return call("POST", *addr+"/api/schemas/"+subject+"/versions", payload)
}

func get(subject string) (*response, error) {
	return call("GET", *addr+"/api/schemas/"+subject, nil)
}

func field(name string, number int, wireType string) map[string]any {
	return map[string]any{"name": name, "number": number, "wireType": wireType}
}

func main() {
	flag.Parse()

	// Wait for the API to become healthy.
	deadline := time.Now().Add(*wait)
	ready := false
	for time.Now().Before(deadline) {
		resp, err := call("GET", *addr+"/healthz", nil)
		if err == nil && resp.status == http.StatusOK {
			ready = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !ready {
		fmt.Printf("FAIL readiness: API at %s not healthy within %s\n", *addr, *wait)
		os.Exit(1)
	}
	fmt.Println("PASS readiness: /healthz OK")

	subject := fmt.Sprintf("smoke-%d", time.Now().UnixNano())

	// Unknown subject is a stable 404.
	resp, err := get(subject)
	check("get unknown subject -> 404 SUBJECT_NOT_FOUND",
		err == nil && resp.status == http.StatusNotFound && resp.errCode() == "SUBJECT_NOT_FOUND",
		resp.status, string(resp.raw), err)

	// First version must be 1.
	resp, err = post(subject, map[string]any{
		"requestId": "v0", "version": 2,
		"fields": []any{field("a", 1, "VARINT")},
	})
	check("first version != 1 -> 409 VERSION_NOT_NEXT currentVersion=0",
		err == nil && resp.status == http.StatusConflict &&
			resp.errCode() == "VERSION_NOT_NEXT" && resp.errField("currentVersion") == float64(0),
		resp.status, string(resp.raw), err)

	// Publish version 1.
	v1 := map[string]any{
		"requestId": "v1", "version": 1,
		"fields": []any{field("a", 1, "VARINT"), field("b", 2, "FIXED64"), field("c", 3, "LENGTH_DELIMITED")},
	}
	resp, err = post(subject, v1)
	createdOK := err == nil && resp.status == http.StatusCreated && resp.body["version"] == float64(1)
	check("publish v1 -> 201", createdOK, resp.status, string(resp.raw), err)
	createdBody := resp.raw

	// GET shows the current version.
	resp, err = get(subject)
	check("get after v1 -> currentVersion 1",
		err == nil && resp.status == http.StatusOK && resp.body["currentVersion"] == float64(1),
		resp.status, string(resp.raw), err)
	v1View := resp.body

	// Identical retry replays the original result byte-for-byte.
	resp, err = post(subject, v1)
	check("same requestId + same content -> replay",
		err == nil && resp.status == http.StatusCreated &&
			resp.header.Get("X-Idempotent-Replay") == "true" && bytes.Equal(resp.raw, createdBody),
		resp.status, string(resp.raw), err)

	// Same requestId, different content conflicts.
	other := map[string]any{
		"requestId": "v1", "version": 1,
		"fields": []any{field("a", 1, "VARINT")},
	}
	resp, err = post(subject, other)
	check("same requestId + different content -> 409 REQUEST_CONFLICT",
		err == nil && resp.status == http.StatusConflict && resp.errCode() == "REQUEST_CONFLICT",
		resp.status, string(resp.raw), err)

	// Skipping a version is rejected and reports the current version.
	resp, err = post(subject, map[string]any{
		"requestId": "skip", "version": 3,
		"fields": []any{field("a", 1, "VARINT"), field("b", 2, "FIXED64"), field("c", 3, "LENGTH_DELIMITED")},
	})
	check("version skip -> 409 VERSION_NOT_NEXT currentVersion=1",
		err == nil && resp.status == http.StatusConflict &&
			resp.errCode() == "VERSION_NOT_NEXT" && resp.errField("currentVersion") == float64(1),
		resp.status, string(resp.raw), err)

	// Deleting a field without retaining its number is rejected and names the
	// lowest offending number.
	resp, err = post(subject, map[string]any{
		"requestId": "drop-no-retain", "version": 2,
		"fields": []any{field("a", 1, "VARINT"), field("b", 2, "FIXED64")},
	})
	check("delete without retain -> 409 NUMBER_NOT_RETAINED firstViolation=3",
		err == nil && resp.status == http.StatusConflict &&
			resp.errCode() == "NUMBER_NOT_RETAINED" && resp.errField("firstViolation") == float64(3),
		resp.status, string(resp.raw), err)

	// Renaming an existing number is rejected.
	resp, err = post(subject, map[string]any{
		"requestId": "rename", "version": 2,
		"fields":          []any{field("renamed", 1, "VARINT"), field("b", 2, "FIXED64"), field("c", 3, "LENGTH_DELIMITED")},
		"reservedNumbers": []int{},
	})
	check("rename existing number -> 409 FIELD_RENAMED firstViolation=1",
		err == nil && resp.status == http.StatusConflict &&
			resp.errCode() == "FIELD_RENAMED" && resp.errField("firstViolation") == float64(1),
		resp.status, string(resp.raw), err)

	// Changing a wire type is rejected.
	resp, err = post(subject, map[string]any{
		"requestId": "retype", "version": 2,
		"fields": []any{field("a", 1, "VARINT"), field("b", 2, "FIXED32"), field("c", 3, "LENGTH_DELIMITED")},
	})
	check("change wire type -> 409 WIRE_TYPE_CHANGED firstViolation=2",
		err == nil && resp.status == http.StatusConflict &&
			resp.errCode() == "WIRE_TYPE_CHANGED" && resp.errField("firstViolation") == float64(2),
		resp.status, string(resp.raw), err)

	// Duplicate field numbers in one submission are rejected.
	resp, err = post(subject, map[string]any{
		"requestId": "dup", "version": 2,
		"fields": []any{field("a", 1, "VARINT"), field("x", 1, "VARINT")},
	})
	check("duplicate field number -> 400 DUPLICATE_FIELD_NUMBER",
		err == nil && resp.status == http.StatusBadRequest && resp.errCode() == "DUPLICATE_FIELD_NUMBER",
		resp.status, string(resp.raw), err)

	// None of the failures above changed the subject.
	resp, err = get(subject)
	check("failed requests left subject unchanged",
		err == nil && reflect.DeepEqual(resp.body, v1View),
		resp.status, string(resp.raw), err)

	// Legal evolution to v2: drop field 3 and retain its number, add field 4.
	resp, err = post(subject, map[string]any{
		"requestId": "v2", "version": 2,
		"fields":          []any{field("a", 1, "VARINT"), field("b", 2, "FIXED64"), field("d", 4, "FIXED32")},
		"reservedNumbers": []int{3},
	})
	check("evolve to v2 (delete+retain, add) -> 201",
		err == nil && resp.status == http.StatusCreated && resp.body["version"] == float64(2),
		resp.status, string(resp.raw), err)

	resp, err = get(subject)
	check("get after v2 -> currentVersion 2, reserved [3]",
		err == nil && resp.status == http.StatusOK && resp.body["currentVersion"] == float64(2) &&
			reflect.DeepEqual(resp.body["reservedNumbers"], []any{float64(3)}),
		resp.status, string(resp.raw), err)
	v2View := resp.body

	// Reusing a tombstoned number is rejected.
	resp, err = post(subject, map[string]any{
		"requestId": "reuse", "version": 3,
		"fields":          []any{field("a", 1, "VARINT"), field("b", 2, "FIXED64"), field("d", 4, "FIXED32"), field("zombie", 3, "VARINT")},
		"reservedNumbers": []int{3},
	})
	check("reuse reserved number -> 409 RESERVED_NUMBER_REUSED firstViolation=3",
		err == nil && resp.status == http.StatusConflict &&
			resp.errCode() == "RESERVED_NUMBER_REUSED" && resp.errField("firstViolation") == float64(3),
		resp.status, string(resp.raw), err)

	// Revoking a tombstoned number is rejected.
	resp, err = post(subject, map[string]any{
		"requestId": "revoke", "version": 3,
		"fields":          []any{field("a", 1, "VARINT"), field("b", 2, "FIXED64"), field("d", 4, "FIXED32")},
		"reservedNumbers": []int{},
	})
	check("revoke reserved number -> 409 RESERVED_NUMBER_REVOKED firstViolation=3",
		err == nil && resp.status == http.StatusConflict &&
			resp.errCode() == "RESERVED_NUMBER_REVOKED" && resp.errField("firstViolation") == float64(3),
		resp.status, string(resp.raw), err)

	// Deleting without retaining still rejected at v3.
	resp, err = post(subject, map[string]any{
		"requestId": "drop4", "version": 3,
		"fields":          []any{field("a", 1, "VARINT"), field("b", 2, "FIXED64")},
		"reservedNumbers": []int{3},
	})
	check("delete field 4 without retain -> 409 NUMBER_NOT_RETAINED firstViolation=4",
		err == nil && resp.status == http.StatusConflict &&
			resp.errCode() == "NUMBER_NOT_RETAINED" && resp.errField("firstViolation") == float64(4),
		resp.status, string(resp.raw), err)

	// Failures still left the subject untouched.
	resp, err = get(subject)
	check("failed evolutions left v2 unchanged",
		err == nil && reflect.DeepEqual(resp.body, v2View),
		resp.status, string(resp.raw), err)

	// Concurrent submissions of the same successor version: exactly one wins.
	const workers = 8
	statuses := make([]int, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := post(subject, map[string]any{
				"requestId": fmt.Sprintf("race-%d", i), "version": 3,
				"fields": []any{
					field("a", 1, "VARINT"), field("b", 2, "FIXED64"), field("d", 4, "FIXED32"),
					field(fmt.Sprintf("extra%d", i), 10+i, "VARINT"),
				},
				"reservedNumbers": []int{3},
			})
			if err != nil {
				statuses[i] = -1
				return
			}
			statuses[i] = resp.status
		}(i)
	}
	wg.Wait()
	created := 0
	for _, st := range statuses {
		if st == http.StatusCreated {
			created++
		}
	}
	check("concurrent successors: exactly one 201", created == 1, statuses)

	resp, err = get(subject)
	check("after race: currentVersion 3, reserved still [3]",
		err == nil && resp.body["currentVersion"] == float64(3) &&
			reflect.DeepEqual(resp.body["reservedNumbers"], []any{float64(3)}),
		resp.status, string(resp.raw), err)

	if failures > 0 {
		fmt.Printf("SMOKE FAILED: %d check(s) failed\n", failures)
		os.Exit(1)
	}
	fmt.Println("SMOKE OK")
}
