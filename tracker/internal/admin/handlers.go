package admin

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// handleHealth returns a small JSON document so probes can confirm the
// admin server is reachable. The version field comes from Deps.Version
// and is empty when not built with -ldflags.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": s.deps.Version,
		"time":    s.deps.Now().UTC().Format(time.RFC3339),
	})
}

// handleStats returns a coarse-grained status snapshot. broker_reqs_per_sec
// is sourced from the metrics subsystem when wired; for now it is null.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	tipSeq, tipHash, hasTip, err := s.deps.Ledger.Tip(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ledger tip: "+err.Error())
		return
	}
	ledger := map[string]any{}
	if hasTip {
		ledger["tip_seq"] = tipSeq
		ledger["tip_hash"] = hex.EncodeToString(tipHash)
	} else {
		ledger["tip_seq"] = nil
		ledger["tip_hash"] = nil
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"connections":         s.deps.PeerCounter.PeerCount(),
		"ledger":              ledger,
		"merkle_root_minutes": s.deps.Config.Ledger.MerkleRootIntervalMin,
		"broker_reqs_per_sec": nil,
	})
}

// handlePeers lists federation peers with link health. When the
// federation subsystem is not wired (admin Deps.Federation is nil) the
// response reports federation_state=disabled with an empty list, so
// operators can tell a misconfigured-tracker apart from a partitioned
// one without consulting logs.
func (s *Server) handlePeers(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Federation == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"peers":            []any{},
			"connected_quic":   s.deps.PeerCounter.PeerCount(),
			"federation_state": "disabled",
		})
		return
	}
	peers := s.deps.Federation.Peers()
	out := make([]map[string]any, 0, len(peers))
	for _, p := range peers {
		row := map[string]any{
			"tracker_id":   hex.EncodeToString(p.TrackerID[:]),
			"pubkey":       hex.EncodeToString(p.PubKey),
			"addr":         p.Addr,
			"region":       p.Region,
			"state":        p.State,
			"health_score": p.HealthScore,
		}
		if !p.Since.IsZero() {
			row["since"] = p.Since.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"peers":            out,
		"connected_quic":   s.deps.PeerCounter.PeerCount(),
		"federation_state": "enabled",
		"listen_addr":      s.deps.Federation.ListenAddr(),
	})
}

// peersAddBody is the operator-supplied JSON for POST /peers/add. All
// fields are hex-or-string with no defaults; missing fields produce 400.
type peersAddBody struct {
	TrackerID string `json:"tracker_id"`
	PubKey    string `json:"pubkey"`
	Addr      string `json:"addr"`
	Region    string `json:"region"`
}

// handlePeersAdd registers an operator-allowlisted peer at runtime.
// 202 is returned on a successful registration because the actual dial
// happens asynchronously in a per-peer goroutine.
func (s *Server) handlePeersAdd(w http.ResponseWriter, r *http.Request) {
	if s.deps.Federation == nil {
		writeError(w, http.StatusNotImplemented, "federation subsystem not configured")
		return
	}
	defer r.Body.Close()
	var body peersAddBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	tid, err := parseHexTrackerID(body.TrackerID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "tracker_id: "+err.Error())
		return
	}
	pk, err := hex.DecodeString(body.PubKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, "pubkey: "+err.Error())
		return
	}
	if len(pk) != 32 {
		writeError(w, http.StatusBadRequest, "pubkey must be 32 bytes")
		return
	}
	if body.Addr == "" {
		writeError(w, http.StatusBadRequest, "addr required")
		return
	}
	req := PeerAddRequest{TrackerID: tid, PubKey: pk, Addr: body.Addr, Region: body.Region}
	switch err := s.deps.Federation.AddPeer(req); {
	case errors.Is(err, ErrPeerExists):
		writeError(w, http.StatusConflict, "peer already registered")
		return
	case err != nil:
		s.deps.Logger.Warn().Err(err).Str("event", "admin_add_peer").Msg("federation rejected AddPeer")
		writeError(w, http.StatusInternalServerError, "add peer: "+err.Error())
		return
	}
	s.deps.Logger.Info().Str("event", "admin_add_peer").Str("tracker_id", body.TrackerID).Msg("operator added peer")
	writeJSON(w, http.StatusAccepted, map[string]any{
		"tracker_id": body.TrackerID,
		"state":      "pending",
	})
}

// peersRemoveBody is the operator-supplied JSON for POST /peers/remove.
type peersRemoveBody struct {
	TrackerID string `json:"tracker_id"`
}

// handlePeersRemove removes a peer from the active set. Federation
// records the depeer reason; admin always reports ReasonOperator via
// the cmd-side adapter.
func (s *Server) handlePeersRemove(w http.ResponseWriter, r *http.Request) {
	if s.deps.Federation == nil {
		writeError(w, http.StatusNotImplemented, "federation subsystem not configured")
		return
	}
	defer r.Body.Close()
	var body peersRemoveBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	tid, err := parseHexTrackerID(body.TrackerID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "tracker_id: "+err.Error())
		return
	}
	switch err := s.deps.Federation.Depeer(tid); {
	case errors.Is(err, ErrPeerUnknown):
		writeError(w, http.StatusNotFound, "peer unknown")
		return
	case err != nil:
		s.deps.Logger.Warn().Err(err).Str("event", "admin_remove_peer").Msg("federation rejected Depeer")
		writeError(w, http.StatusInternalServerError, "depeer: "+err.Error())
		return
	}
	s.deps.Logger.Info().Str("event", "admin_remove_peer").Str("tracker_id", body.TrackerID).Msg("operator removed peer")
	writeJSON(w, http.StatusOK, map[string]any{
		"tracker_id": body.TrackerID,
		"depeered":   true,
	})
}

// handleIdentity returns whatever is known about the identity. Both the
// registry record and the signed balance are optional; an identity
// totally unknown to both surfaces produces 404.
func (s *Server) handleIdentity(w http.ResponseWriter, r *http.Request) {
	id, err := parseHexID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad id: "+err.Error())
		return
	}

	out := map[string]any{"identity_id": hex.EncodeToString(id[:])}
	known := false

	if rec, ok := s.deps.Registry.Get(id); ok {
		known = true
		out["seeder"] = map[string]any{
			"available":         rec.Available,
			"headroom_estimate": rec.HeadroomEstimate,
			"reputation_score":  rec.ReputationScore,
			"load":              rec.Load,
			"last_heartbeat":    rec.LastHeartbeat.UTC().Format(time.RFC3339),
			"models":            rec.Capabilities.Models,
		}
	}

	bal, err := s.deps.Ledger.SignedBalance(r.Context(), id[:])
	switch {
	case err == nil && bal != nil && bal.GetBody() != nil:
		known = true
		body := bal.GetBody()
		out["balance"] = map[string]any{
			"credits":       body.GetCredits(),
			"chain_tip_seq": body.GetChainTipSeq(),
			"issued_at":     body.GetIssuedAt(),
			"expires_at":    body.GetExpiresAt(),
		}
	case err != nil:
		// SignedBalance failure on a fully-unknown identity is benign
		// (no rows). Surface the error only when we're already going
		// to return 200 because the registry knew the id.
		if known {
			out["balance_error"] = err.Error()
		}
	}

	if !known {
		writeError(w, http.StatusNotFound, "identity not found")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleMaintenance triggers a graceful shutdown via the cmd-supplied
// callback. The handler returns 202 immediately; the actual drain runs
// asynchronously in cmd/run_cmd.
func (s *Server) handleMaintenance(w http.ResponseWriter, _ *http.Request) {
	s.deps.Logger.Info().Str("event", "admin_maintenance").Msg("operator triggered shutdown")
	go s.deps.TriggerMaintenance()
	writeJSON(w, http.StatusAccepted, map[string]any{"draining": true})
}
