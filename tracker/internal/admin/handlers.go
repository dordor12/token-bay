package admin

import (
	"encoding/hex"
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

// handlePeers lists federation peers. Federation is not yet implemented;
// the response is honest about that so operators don't conclude their
// peering is broken.
func (s *Server) handlePeers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"peers":            []any{},
		"connected_quic":   s.deps.PeerCounter.PeerCount(),
		"federation_state": "not_implemented",
	})
}

// handlePeersUnimplemented covers POST /peers/add and /peers/remove until
// the federation subsystem ships those mutations.
func (s *Server) handlePeersUnimplemented(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotImplemented, "federation subsystem not yet implemented")
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
