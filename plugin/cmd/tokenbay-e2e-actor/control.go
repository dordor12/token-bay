package main

import (
	"encoding/json"
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
// (json shapes matching the e2e driver's SeederCtl); the consumer role's
// routes land in a later task.
func (a *Actor) registerRoleRoutes(mux *http.ServeMux) {
	if a.seeder != nil {
		mux.HandleFunc("/config", a.handleSeederConfig)
		mux.HandleFunc("/offers/last", a.handleLastOffer)
		mux.HandleFunc("/usage/last", a.handleLastUsage)
	}
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
