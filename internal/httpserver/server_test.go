package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"schema-registry/internal/registry"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	store, err := registry.OpenStore(filepath.Join(t.TempDir(), "registry.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return httptest.NewServer(New(registry.NewService(store)))
}

func TestPublishAndGetEndToEnd(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/schemas/telemetry.device/versions", "application/json",
		strings.NewReader(`{"requestId":"r1","version":1,"fields":[{"name":"temp","number":1,"wireType":"FIXED32"}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}

	resp2, err := http.Get(srv.URL + "/api/schemas/telemetry.device")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp2.Body.Close()
	var schema registry.SchemaResponse
	if err := json.NewDecoder(resp2.Body).Decode(&schema); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if schema.Subject != "telemetry.device" || schema.CurrentVersion != 1 || len(schema.Fields) != 1 {
		t.Fatalf("schema = %+v", schema)
	}
	if schema.ReservedNumbers == nil {
		t.Fatal("reservedNumbers should be [], got null")
	}
}

func TestReplayHeader(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	body := `{"requestId":"r1","version":1,"fields":[{"name":"a","number":1,"wireType":"VARINT"}]}`

	resp, _ := http.Post(srv.URL+"/api/schemas/s/versions", "application/json", strings.NewReader(body))
	resp.Body.Close()
	if resp.Header.Get("X-Idempotent-Replay") != "" {
		t.Fatal("first publish must not be marked as replay")
	}

	resp, _ = http.Post(srv.URL+"/api/schemas/s/versions", "application/json", strings.NewReader(body))
	resp.Body.Close()
	if resp.Header.Get("X-Idempotent-Replay") != "true" {
		t.Fatal("retry must be marked as replay")
	}
}

func TestGetUnknownSubjectReturns404(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/schemas/ghost")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	var er registry.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if er.Error.Code != registry.CodeSubjectNotFound {
		t.Fatalf("code = %q, want SUBJECT_NOT_FOUND", er.Error.Code)
	}
}

func TestMalformedBodyReturns400(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()

	for _, body := range []string{
		`{not json`,
		`{"requestId":"r1","version":1,"fields":[],"unknown":true}`,
		`{"requestId":"r1","version":1,"fields":[]} trailing`,
	} {
		resp, err := http.Post(srv.URL+"/api/schemas/s/versions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		var er registry.ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&er)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want 400", body, resp.StatusCode)
		}
		if er.Error.Code != registry.CodeInvalidRequest {
			t.Fatalf("body %q: code = %q, want INVALID_REQUEST", body, er.Error.Code)
		}
	}
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t)
	defer srv.Close()
	for _, path := range []string{"/healthz", "/api/healthz"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", path, resp.StatusCode)
		}
	}
}
