package broker

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

// AdminHandler returns an http.Handler covering /broker/* admin routes.
// cmd/run_cmd will mount this under the tracker's admin port when the admin
// server lands.
func (s *Subsystems) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /broker/inflight", s.handleListInflight)
	mux.HandleFunc("GET /broker/inflight/{request_id}", s.handleGetInflight)
	mux.HandleFunc("GET /broker/reservations", s.handleListReservations)
	mux.HandleFunc("POST /broker/reservations/release/{request_id}", s.handleForceReleaseReservation)
	mux.HandleFunc("POST /broker/inflight/fail/{request_id}", s.handleForceFailInflight)
	return mux
}

// handleListInflight lists all in-flight requests as JSON.
func (s *Subsystems) handleListInflight(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	snap := s.Broker.mgr.Inflight.Snapshot()
	out := make([]map[string]any, 0, len(snap))
	for _, sum := range snap {
		out = append(out, map[string]any{
			"request_id":  hex.EncodeToString(sum.RequestID[:]),
			"consumer_id": hex.EncodeToString(sum.ConsumerID[:]),
			"state":       sum.State.String(),
			"age_seconds": now.Sub(sum.StartedAt).Seconds(),
		})
	}
	adminWriteJSON(w, http.StatusOK, out)
}

// handleGetInflight returns per-request detail; 404 if not found.
func (s *Subsystems) handleGetInflight(w http.ResponseWriter, r *http.Request) {
	reqID, err := parseHex16(r.PathValue("request_id"))
	if err != nil {
		http.Error(w, "bad request_id", http.StatusBadRequest)
		return
	}
	req, ok := s.Broker.mgr.Inflight.Get(reqID)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	slot, hasSlot := s.Broker.mgr.Reservations.Get(reqID)

	var seederHex string
	if req.AssignedSeeder != ([32]byte{}) {
		seederHex = hex.EncodeToString(req.AssignedSeeder[:])
	}
	detail := map[string]any{
		"request_id":  hex.EncodeToString(req.RequestID[:]),
		"consumer_id": hex.EncodeToString(req.ConsumerID[:]),
		"state":       req.State.String(),
		"seeder_id":   seederHex,
	}
	if hasSlot {
		detail["reservation_amount"] = slot.Amount
	}
	adminWriteJSON(w, http.StatusOK, detail)
}

// handleListReservations lists per-consumer reserved totals and slot details.
func (s *Subsystems) handleListReservations(w http.ResponseWriter, _ *http.Request) {
	type slotDTO struct {
		RequestID string `json:"request_id"`
		Amount    uint64 `json:"amount"`
		ExpiresAt int64  `json:"expires_at"`
	}
	type consumerDTO struct {
		ConsumerID string    `json:"consumer_id"`
		Total      uint64    `json:"total"`
		Slots      []slotDTO `json:"slots"`
	}

	snap := s.Broker.mgr.Reservations.Snapshot()
	byConsumer := make(map[[32]byte]*consumerDTO)
	for _, slot := range snap {
		cid := slot.ConsumerID
		if _, ok := byConsumer[cid]; !ok {
			byConsumer[cid] = &consumerDTO{
				ConsumerID: hex.EncodeToString(cid[:]),
				Total:      s.Broker.mgr.Reservations.Reserved(cid),
			}
		}
		byConsumer[cid].Slots = append(byConsumer[cid].Slots, slotDTO{
			RequestID: hex.EncodeToString(slot.ReqID[:]),
			Amount:    slot.Amount,
			ExpiresAt: slot.ExpiresAt.Unix(),
		})
	}

	out := make([]*consumerDTO, 0, len(byConsumer))
	for _, dto := range byConsumer {
		out = append(out, dto)
	}
	adminWriteJSON(w, http.StatusOK, out)
}

// handleForceReleaseReservation forces a reservation release and emits an
// audit log entry.
func (s *Subsystems) handleForceReleaseReservation(w http.ResponseWriter, r *http.Request) {
	reqID, err := parseHex16(r.PathValue("request_id"))
	if err != nil {
		http.Error(w, "bad request_id", http.StatusBadRequest)
		return
	}
	reqIDHex := hex.EncodeToString(reqID[:])
	_, _, ok := s.Broker.mgr.Reservations.Release(reqID)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.Broker.deps.Logger.Info().
		Str("event", "broker_admin_override").
		Str("endpoint", "reservations/release").
		Str("request_id", reqIDHex).
		Msg("")
	adminWriteJSON(w, http.StatusOK, map[string]any{"released": true, "request_id": reqIDHex})
}

// handleForceFailInflight forces an in-flight request to StateFailed and
// emits an audit log entry.
func (s *Subsystems) handleForceFailInflight(w http.ResponseWriter, r *http.Request) {
	reqID, err := parseHex16(r.PathValue("request_id"))
	if err != nil {
		http.Error(w, "bad request_id", http.StatusBadRequest)
		return
	}
	reqIDHex := hex.EncodeToString(reqID[:])
	prev, ok := s.Broker.mgr.Inflight.ForceFail(reqID, time.Now())
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	s.Broker.deps.Logger.Info().
		Str("event", "broker_admin_override").
		Str("endpoint", "inflight/fail").
		Str("request_id", reqIDHex).
		Str("prev_state", prev.String()).
		Msg("")
	adminWriteJSON(w, http.StatusOK, map[string]any{"failed": true, "request_id": reqIDHex})
}

// adminWriteJSON writes a JSON response with the given status code.
func adminWriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// parseHex16 decodes a 32-hex-char (16-byte) request_id from the URL.
func parseHex16(s string) ([16]byte, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return [16]byte{}, err
	}
	if len(b) != 16 {
		return [16]byte{}, &hexLenError{got: len(b), want: 16}
	}
	var id [16]byte
	copy(id[:], b)
	return id, nil
}

type hexLenError struct{ got, want int }

func (e *hexLenError) Error() string {
	return "hex ID: wrong length"
}
