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
// endpoints. It is intentionally empty in the skeleton; later tasks branch
// on a.opts.Role to wire seeder/consumer routes.
func (a *Actor) registerRoleRoutes(_ *http.ServeMux) {}

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
