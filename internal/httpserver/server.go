// Package httpserver exposes the schema registry over HTTP.
package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"schema-registry/internal/registry"
)

// New builds the HTTP handler with all API routes.
func New(svc *registry.Service) http.Handler {
	h := &handlers{svc: svc}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/schemas/{subject}/versions", h.publish)
	mux.HandleFunc("GET /api/schemas/{subject}", h.getSchema)
	mux.HandleFunc("GET /healthz", h.health)
	mux.HandleFunc("GET /api/healthz", h.health)
	return mux
}

type handlers struct {
	svc *registry.Service
}

func (h *handlers) publish(w http.ResponseWriter, r *http.Request) {
	subject := r.PathValue("subject")
	if subject == "" {
		writeError(w, http.StatusBadRequest, registry.ErrorBody{
			Code:    registry.CodeInvalidRequest,
			Message: "subject is required",
		})
		return
	}

	body := http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	var req registry.PublishRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, registry.ErrorBody{
			Code:    registry.CodeInvalidRequest,
			Message: "invalid JSON body: " + err.Error(),
		})
		return
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, registry.ErrorBody{
			Code:    registry.CodeInvalidRequest,
			Message: "request body must contain a single JSON object",
		})
		return
	}

	res := h.svc.Publish(subject, req)
	w.Header().Set("Content-Type", "application/json")
	if res.Replay {
		w.Header().Set("X-Idempotent-Replay", "true")
	}
	w.WriteHeader(res.Status)
	_, _ = w.Write(res.Body)
}

func (h *handlers) getSchema(w http.ResponseWriter, r *http.Request) {
	subject := r.PathValue("subject")
	resp, ok := h.svc.Get(subject)
	if !ok {
		writeError(w, http.StatusNotFound, registry.ErrorBody{
			Code:    registry.CodeSubjectNotFound,
			Message: "subject " + subject + " has no published schema version",
		})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handlers) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		status = http.StatusInternalServerError
		data, _ = json.Marshal(registry.ErrorResponse{Error: registry.ErrorBody{
			Code:    registry.CodeInternal,
			Message: err.Error(),
		}})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func writeError(w http.ResponseWriter, status int, body registry.ErrorBody) {
	writeJSON(w, status, registry.ErrorResponse{Error: body})
}
