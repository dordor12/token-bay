package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// controlServer exposes the fedactor's HTTP control API. Bodies are simple
// JSON; every handler is synchronous and returns 200 on success or a plain
// error status otherwise. Intended for the e2e driver, not production use.
type controlServer struct {
	actor *Actor
}

func newControlServer(a *Actor) *controlServer { return &controlServer{actor: a} }

// mux wires the routes.
func (s *controlServer) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/handshake", s.handleHandshake)
	m.HandleFunc("/send/root-attestation", s.handleRootAttestation)
	m.HandleFunc("/send/equivocation-evidence", s.handleEquivocationEvidence)
	m.HandleFunc("/send/revocation", s.handleRevocation)
	m.HandleFunc("/received", s.handleReceived)
	return m
}

type handshakeReq struct {
	Addr      string `json:"addr"`
	PubKeyHex string `json:"pubkey_hex"`
}

func (s *controlServer) handleHandshake(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	var req handshakeReq
	if !decode(w, r, &req) {
		return
	}
	if err := s.actor.handshake(r.Context(), req.Addr, req.PubKeyHex); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

type rootAttestationReq struct {
	Hour    uint64 `json:"hour"`
	RootHex string `json:"root_hex"`
}

func (s *controlServer) handleRootAttestation(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	var req rootAttestationReq
	if !decode(w, r, &req) {
		return
	}
	if err := s.actor.sendRootAttestation(r.Context(), req.Hour, req.RootHex); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"status": "sent"})
}

type equivocationReq struct {
	Hour     uint64 `json:"hour"`
	RootAHex string `json:"root_a_hex"`
	RootBHex string `json:"root_b_hex"`
}

func (s *controlServer) handleEquivocationEvidence(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	var req equivocationReq
	if !decode(w, r, &req) {
		return
	}
	if err := s.actor.sendEquivocationEvidence(r.Context(), req.Hour, req.RootAHex, req.RootBHex); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"status": "sent"})
}

type revocationReq struct {
	IdentityHex string `json:"identity_hex"`
	Reason      int    `json:"reason"`
}

func (s *controlServer) handleRevocation(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	var req revocationReq
	if !decode(w, r, &req) {
		return
	}
	if err := s.actor.sendRevocation(r.Context(), req.IdentityHex, req.Reason); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"status": "sent"})
}

func (s *controlServer) handleReceived(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.actor.receivedRecords())
}

// --- helpers ---

func requirePost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	return true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// listenAndServe runs the control server until ctx is canceled.
func (s *controlServer) listenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.mux(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
