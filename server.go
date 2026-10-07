package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
)

type api struct {
	store *Store
}

func newMux(a *api) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("POST /api/schemas/{subject}/versions", a.postVersion)
	mux.HandleFunc("GET /api/schemas/{subject}", a.getSubject)
	return mux
}

func (a *api) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// postVersion handles POST /api/schemas/{subject}/versions.
func (a *api) postVersion(w http.ResponseWriter, r *http.Request) {
	subject := r.PathValue("subject")
	if !subjectRe.MatchString(subject) {
		writeError(w, http.StatusBadRequest, &APIError{Code: CodeValidation, Message: "invalid subject name " + strconv.Quote(subject)})
		return
	}
	var req SubmitRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		msg := "invalid JSON body"
		if errors.As(err, &maxErr) {
			msg = "request body too large"
		}
		writeError(w, http.StatusBadRequest, &APIError{Code: CodeValidation, Message: msg})
		return
	}
	if apiErr := validateShape(&req); apiErr != nil {
		writeError(w, http.StatusBadRequest, apiErr)
		return
	}
	status, body, replayed := a.store.Submit(subject, &req)
	if replayed {
		w.Header().Set("X-Idempotent-Replay", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// getSubject handles GET /api/schemas/{subject}.
func (a *api) getSubject(w http.ResponseWriter, r *http.Request) {
	subject := r.PathValue("subject")
	if !subjectRe.MatchString(subject) {
		writeError(w, http.StatusBadRequest, &APIError{Code: CodeValidation, Message: "invalid subject name " + strconv.Quote(subject)})
		return
	}
	view, ok := a.store.Get(subject)
	if !ok {
		writeError(w, http.StatusNotFound, &APIError{Code: CodeSubjectNotFound, Message: "subject " + strconv.Quote(subject) + " not found"})
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, e *APIError) {
	writeJSON(w, status, errorBody{Error: e})
}
