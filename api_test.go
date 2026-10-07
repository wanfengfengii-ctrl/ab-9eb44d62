package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	srv := httptest.NewServer(newMux(&api{store: store}))
	t.Cleanup(srv.Close)
	return srv
}

func doReq(t *testing.T, method, url, body string) (int, map[string]any, http.Header) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("response not JSON: %s", raw)
		}
	}
	return resp.StatusCode, parsed, resp.Header
}

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	st, body, _ := doReq(t, "GET", srv.URL+"/healthz", "")
	if st != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("healthz: status=%d body=%v", st, body)
	}
}

func TestGetUnknownSubject(t *testing.T) {
	srv := newTestServer(t)
	st, body, _ := doReq(t, "GET", srv.URL+"/api/schemas/nope", "")
	if st != http.StatusNotFound || errCode(body) != CodeSubjectNotFound {
		t.Fatalf("status=%d body=%v", st, body)
	}
}

func TestPublishGetAndReplayOverHTTP(t *testing.T) {
	srv := newTestServer(t)
	v1 := `{"requestId":"req-1","version":1,"fields":[
		{"name":"temp","number":1,"wireType":"varint"},
		{"name":"hum","number":2,"wireType":"FIXED64"},
		{"name":"raw","number":3,"wireType":"length-delimited"}]}`

	st, body, _ := doReq(t, "POST", srv.URL+"/api/schemas/dev/versions", v1)
	if st != http.StatusCreated {
		t.Fatalf("publish v1: status=%d body=%v", st, body)
	}
	if body["version"].(float64) != 1 {
		t.Fatalf("version = %v", body["version"])
	}

	// GET returns current version and cumulative reserved numbers.
	st, view, _ := doReq(t, "GET", srv.URL+"/api/schemas/dev", "")
	if st != http.StatusOK || view["currentVersion"].(float64) != 1 {
		t.Fatalf("get: status=%d body=%v", st, view)
	}
	if rn, ok := view["reservedNumbers"].([]any); !ok || len(rn) != 0 {
		t.Fatalf("reservedNumbers = %v", view["reservedNumbers"])
	}

	// Identical retry replays the original result.
	st, body2, hdr := doReq(t, "POST", srv.URL+"/api/schemas/dev/versions", v1)
	if st != http.StatusCreated || hdr.Get("X-Idempotent-Replay") != "true" {
		t.Fatalf("replay: status=%d header=%q", st, hdr.Get("X-Idempotent-Replay"))
	}
	if body2["version"].(float64) != 1 {
		t.Fatalf("replayed body = %v", body2)
	}

	// Same requestId, different content conflicts.
	st, body3, _ := doReq(t, "POST", srv.URL+"/api/schemas/dev/versions",
		`{"requestId":"req-1","version":1,"fields":[{"name":"x","number":9,"wireType":"VARINT"}]}`)
	if st != http.StatusConflict || errCode(body3) != CodeRequestConflict {
		t.Fatalf("conflict: status=%d body=%v", st, body3)
	}
}

func TestEvolutionErrorsOverHTTP(t *testing.T) {
	srv := newTestServer(t)
	mustPost := func(payload string, wantStatus int, wantCode string) map[string]any {
		t.Helper()
		st, body, _ := doReq(t, "POST", srv.URL+"/api/schemas/dev/versions", payload)
		if st != wantStatus {
			t.Fatalf("status=%d want %d body=%v", st, wantStatus, body)
		}
		if wantCode != "" && errCode(body) != wantCode {
			t.Fatalf("code=%q want %q body=%v", errCode(body), wantCode, body)
		}
		return body
	}

	mustPost(`{"requestId":"r1","version":1,"fields":[
		{"name":"a","number":1,"wireType":"VARINT"},
		{"name":"b","number":2,"wireType":"FIXED64"}]}`, http.StatusCreated, "")

	// Skip a version.
	body := mustPost(`{"requestId":"r2","version":3,"fields":[
		{"name":"a","number":1,"wireType":"VARINT"},
		{"name":"b","number":2,"wireType":"FIXED64"}]}`, http.StatusConflict, CodeVersionNotNext)
	if body["error"].(map[string]any)["currentVersion"].(float64) != 1 {
		t.Fatalf("currentVersion in error = %v", body)
	}

	// Delete without retaining.
	body = mustPost(`{"requestId":"r3","version":2,"fields":[
		{"name":"a","number":1,"wireType":"VARINT"}]}`, http.StatusConflict, CodeNumberNotRetained)
	if fv := body["error"].(map[string]any)["firstViolation"].(float64); fv != 2 {
		t.Fatalf("firstViolation = %v, want 2", fv)
	}

	// Failed requests did not change the subject.
	_, view, _ := doReq(t, "GET", srv.URL+"/api/schemas/dev", "")
	if view["currentVersion"].(float64) != 1 {
		t.Fatalf("currentVersion changed by failed requests: %v", view)
	}

	// Proper evolution.
	mustPost(`{"requestId":"r4","version":2,"fields":[
		{"name":"a","number":1,"wireType":"VARINT"},
		{"name":"c","number":4,"wireType":"FIXED32"}],
		"reservedNumbers":[2]}`, http.StatusCreated, "")

	_, view, _ = doReq(t, "GET", srv.URL+"/api/schemas/dev", "")
	rn := view["reservedNumbers"].([]any)
	if len(rn) != 1 || rn[0].(float64) != 2 {
		t.Fatalf("reservedNumbers = %v", rn)
	}
}

func TestMalformedRequests(t *testing.T) {
	srv := newTestServer(t)

	st, body, _ := doReq(t, "POST", srv.URL+"/api/schemas/dev/versions", `{not json`)
	if st != http.StatusBadRequest || errCode(body) != CodeValidation {
		t.Fatalf("bad json: status=%d body=%v", st, body)
	}

	st, body, _ = doReq(t, "POST", srv.URL+"/api/schemas/dev/versions",
		`{"version":1,"fields":[]}`)
	if st != http.StatusBadRequest || errCode(body) != CodeValidation {
		t.Fatalf("missing requestId: status=%d body=%v", st, body)
	}

	st, body, _ = doReq(t, "POST", srv.URL+"/api/schemas/dev/versions",
		`{"requestId":"r","version":1,"fields":[{"name":"a","number":1,"wireType":"VARINT"},{"name":"b","number":1,"wireType":"VARINT"}]}`)
	if st != http.StatusBadRequest || errCode(body) != CodeDuplicateNumber {
		t.Fatalf("dup number: status=%d body=%v", st, body)
	}
}
