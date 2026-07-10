package main

import (
	"encoding/json"
	"io"
	"net/http"
)

// mux builds the actor's HTTP control surface. The base routes here are
// role-agnostic; later tasks extend the surface with role-specific
// endpoints (seeder /config, consumer /request, ...) by registering them
// on the returned mux via registerRoleRoutes.
func (a *Actor) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", a.handleHealthz)
	mux.HandleFunc("/identity", a.handleIdentity)
	a.registerRoleRoutes(mux)
	return mux
}

// registerRoleRoutes is the extension seam for role-specific control
// endpoints. The seeder role registers /config, /offers/last, /usage/last
// (driver SeederCtl shapes); the consumer role registers /config, /request,
// /settlement/last, /transfer, /balance (driver ConsumerCtl shapes). With
// both roles active, /config sniffs the body keys and routes to each role.
func (a *Actor) registerRoleRoutes(mux *http.ServeMux) {
	switch {
	case a.seeder != nil && a.consumer != nil:
		mux.HandleFunc("/config", a.handleBothConfig)
	case a.seeder != nil:
		mux.HandleFunc("/config", a.handleSeederConfig)
	case a.consumer != nil:
		mux.HandleFunc("/config", a.handleConsumerConfig)
	}
	if a.seeder != nil {
		mux.HandleFunc("/offers/last", a.handleLastOffer)
		mux.HandleFunc("/usage/last", a.handleLastUsage)
	}
	if a.consumer != nil {
		mux.HandleFunc("/request", a.handleConsumerRequest)
		mux.HandleFunc("/settlement/last", a.handleLastSettlement)
		mux.HandleFunc("/transfer", a.handleTransfer)
		mux.HandleFunc("/balance", a.handleBalance)
	}
}

// handleConsumerConfig accepts the driver's ConsumerConfig body: the settle
// (counter-sign pushed settlements) and dial (dial assigned seeder tunnels)
// toggles. Omitted fields reset to the default (true).
func (a *Actor) handleConsumerConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var cfg consumerConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "bad config body: "+err.Error(), http.StatusBadRequest)
		return
	}
	a.consumer.setConfig(cfg)
	w.WriteHeader(http.StatusNoContent)
}

// handleBothConfig serves /config when BOTH roles are active: the driver's
// SeederConfig and ConsumerConfig shapes share the path, so route by which
// role's keys the body carries (either, or both, may apply).
func (a *Actor) handleBothConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var keys map[string]json.RawMessage
	raw, err := io.ReadAll(r.Body)
	if err == nil {
		err = json.Unmarshal(raw, &keys)
	}
	if err != nil {
		http.Error(w, "bad config body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if hasAnyKey(keys, "available", "headroom", "models", "max_context", "tiers", "sse_body") {
		var cfg seederConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			http.Error(w, "bad seeder config: "+err.Error(), http.StatusBadRequest)
			return
		}
		a.seeder.setConfig(cfg)
	}
	if hasAnyKey(keys, "settle", "dial") {
		var cfg consumerConfig
		if err := json.Unmarshal(raw, &cfg); err != nil {
			http.Error(w, "bad consumer config: "+err.Error(), http.StatusBadRequest)
			return
		}
		a.consumer.setConfig(cfg)
	}
	w.WriteHeader(http.StatusNoContent)
}

func hasAnyKey(m map[string]json.RawMessage, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// handleConsumerRequest runs the full consumer flow (Balance → proof →
// envelope → BrokerRequest → optional tunnel dial) and returns the driver
// RequestResult shape.
func (a *Actor) handleConsumerRequest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !a.ready() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	var spec requestSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	res := a.consumer.runRequest(r.Context(), spec)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// handleLastSettlement returns the last SettlementHandler outcome
// (zero-value JSON when none has arrived yet).
func (a *Actor) handleLastSettlement(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a.consumer.lastSettlementSnapshot())
}

// handleTransfer drives a cross-region TransferRequest against tracker B and
// returns the driver TransferResult shape.
func (a *Actor) handleTransfer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if !a.ready() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	var spec transferSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		http.Error(w, "bad transfer body: "+err.Error(), http.StatusBadRequest)
		return
	}
	res := a.consumer.runTransfer(r.Context(), spec)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// handleBalance returns the driver ActorBalance shape from a fresh Balance
// RPC under the enroll id.
func (a *Actor) handleBalance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if !a.ready() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	bal, err := a.consumer.fetchBalance(r.Context())
	if err != nil {
		http.Error(w, "balance RPC: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(bal)
}

// handleSeederConfig accepts the driver's SeederConfig body: the advertised
// availability/headroom/models/tiers plus the canned SSE body to serve.
func (a *Actor) handleSeederConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var cfg seederConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, "bad config body: "+err.Error(), http.StatusBadRequest)
		return
	}
	a.seeder.setConfig(cfg)
	w.WriteHeader(http.StatusNoContent)
}

// handleLastOffer returns the last offer the OfferHandler decided on
// (zero-value JSON when none has arrived yet).
func (a *Actor) handleLastOffer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a.seeder.lastOfferSnapshot())
}

// handleLastUsage returns the last usage report sent (zero-value JSON when
// none has been sent yet).
func (a *Actor) handleLastUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a.seeder.lastUsageSnapshot())
}

// handleHealthz returns 200 once the actor has connected AND enrolled,
// otherwise 503. This is the readiness signal the compose harness and the
// e2e driver poll on.
func (a *Actor) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	if !a.ready() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// identityResponse is the /identity payload. identity_id_hex is the
// tracker-issued enroll id (SPKI-hash identity), not sha256(rawPubkey).
type identityResponse struct {
	IdentityIDHex string `json:"identity_id_hex"`
	PubkeyHex     string `json:"pubkey_hex"`
}

func (a *Actor) handleIdentity(w http.ResponseWriter, _ *http.Request) {
	idHex, pubHex := a.identitySnapshot()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(identityResponse{
		IdentityIDHex: idHex,
		PubkeyHex:     pubHex,
	})
}
