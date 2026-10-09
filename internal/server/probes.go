package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"yggpeers/internal/coordinator"
	"yggpeers/internal/protocol"
	"yggpeers/internal/store"
)

func (s *Server) registerProbeRoutes() {
	s.mux.HandleFunc("POST "+protocol.CheckInPath, s.probeCheckIn)
	s.mux.HandleFunc("POST "+protocol.ResultsPath, s.probeResults)
}

func (s *Server) authenticatedProbe(w http.ResponseWriter, r *http.Request) (store.Probe, bool) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		w.Header().Set("WWW-Authenticate", "Bearer")
		jsonError(w, "probe credential required", http.StatusUnauthorized)
		return store.Probe{}, false
	}
	probe, err := s.store.AuthenticateProbe(parts[1])
	if err != nil {
		if errors.Is(err, store.ErrUnauthorized) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			jsonError(w, "invalid or disabled probe credential", http.StatusUnauthorized)
		} else {
			log.Printf("authenticate probe: %v", err)
			jsonError(w, "internal error", http.StatusInternalServerError)
		}
		return store.Probe{}, false
	}
	return probe, true
}

func decodeProbeRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		jsonError(w, "invalid request JSON", http.StatusBadRequest)
		return false
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		jsonError(w, "request must contain exactly one JSON object", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) probeCheckIn(w http.ResponseWriter, r *http.Request) {
	p, ok := s.authenticatedProbe(w, r)
	if !ok {
		return
	}
	var request protocol.CheckInRequest
	if !decodeProbeRequest(w, r, &request) {
		return
	}
	work, err := s.store.CheckInProbe(p.ID, request.CheckDarknet, time.Now())
	if err != nil {
		writeProbeError(w, err)
		return
	}
	json.NewEncoder(w).Encode(protocol.WorkResponse{
		ProbeID:   p.ID,
		PollAfter: coordinator.PollInterval(s.cfg.CheckInterval).Milliseconds(),
		Work:      work,
	})
}

func (s *Server) probeResults(w http.ResponseWriter, r *http.Request) {
	p, ok := s.authenticatedProbe(w, r)
	if !ok {
		return
	}
	var request protocol.ReportRequest
	if !decodeProbeRequest(w, r, &request) {
		return
	}
	accepted, err := s.store.SubmitProbeResults(p.ID, request, time.Now())
	if err != nil {
		writeProbeError(w, err)
		return
	}
	json.NewEncoder(w).Encode(protocol.ReportResponse{Accepted: accepted})
}

func writeProbeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrUnauthorized):
		jsonError(w, err.Error(), http.StatusUnauthorized)
	case errors.Is(err, store.ErrInvalidReport):
		jsonError(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, store.ErrConflict):
		jsonError(w, err.Error(), http.StatusConflict)
	case errors.Is(err, store.ErrRoundClosed):
		jsonError(w, err.Error(), http.StatusGone)
	default:
		log.Printf("probe API: %v", err)
		jsonError(w, "internal error", http.StatusInternalServerError)
	}
}
