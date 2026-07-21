package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Service is the machine-side orchestration the HTTP layer exposes. It is
// implemented by internal/daemon; keeping it an interface keeps the transport
// (this package) free of business logic and lets the server be tested with a fake.
type Service interface {
	Version() VersionResponse
	Status(ctx context.Context) (StatusResponse, error)
	Snapshots(ctx context.Context, slot string) ([]SnapshotInfo, error)
	Promote(ctx context.Context) (PromoteResponse, error)
	Snapshot(ctx context.Context, req SnapshotRequest) (OpResponse, error)
	Restore(ctx context.Context, req RestoreRequest) (OpResponse, error)
	Reconcile(ctx context.Context) (ReconcileResponse, error)
	Up(ctx context.Context) (StatusResponse, error)
	Down(ctx context.Context) (OpResponse, error)
}

// Server adapts a Service to the HTTP/JSON contract with bearer-token auth.
type Server struct {
	svc   Service
	token string
}

// NewServer returns an http.Handler serving the v1 API. A non-empty token is
// required on every route except /v1/healthz (an unauthenticated liveness probe).
func NewServer(svc Service, token string) http.Handler {
	s := &Server{svc: svc, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", s.health)
	mux.HandleFunc("GET /v1/version", s.auth(s.version))
	mux.HandleFunc("GET /v1/status", s.auth(s.status))
	mux.HandleFunc("GET /v1/snapshots", s.auth(s.snapshots))
	mux.HandleFunc("POST /v1/promote", s.auth(s.promote))
	mux.HandleFunc("POST /v1/snapshot", s.auth(s.snapshot))
	mux.HandleFunc("POST /v1/restore", s.auth(s.restore))
	mux.HandleFunc("POST /v1/reconcile", s.auth(s.reconcile))
	mux.HandleFunc("POST /v1/up", s.auth(s.up))
	mux.HandleFunc("POST /v1/down", s.auth(s.down))
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.token == "" || got != s.token {
			writeErr(w, http.StatusUnauthorized, errors.New("unauthorized"))
			return
		}
		next(w, r)
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{OK: true})
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Version())
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	st, err := s.svc.Status(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) snapshots(w http.ResponseWriter, r *http.Request) {
	slot := r.URL.Query().Get("slot")
	snaps, err := s.svc.Snapshots(r.Context(), slot)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, SnapshotsResponse{Slot: slot, Snapshots: snaps})
}

func (s *Server) promote(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.Promote(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	var req SnapshotRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.svc.Snapshot(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	var req RestoreRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.svc.Restore(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) reconcile(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.Reconcile(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) up(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.Up(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) down(w http.ResponseWriter, r *http.Request) {
	res, err := s.svc.Down(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, errorBody{Error: err.Error()})
}
