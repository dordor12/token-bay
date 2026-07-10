//go:build e2e

package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Admin is a bearer-authed HTTP client for a single tracker's admin API
// (tracker/internal/admin). BaseURL is e.g. "http://localhost:9090" (or
// "http://localhost:9091" for tracker-b, per the compose port mapping in
// docs/superpowers/plans/2026-07-08-tracker-e2e-testing.md Task 22).
//
// Response structs below are decoded field-for-field against the REAL
// admin handlers in tracker/internal/admin/handlers.go — not guessed.
type Admin struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewAdmin returns an Admin client with a sane default request timeout.
func NewAdmin(baseURL, token string) *Admin {
	return &Admin{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// AdminError is returned for any non-2xx admin response. Scenario tests
// can type-assert on it to inspect StatusCode/Body (e.g. a 404 from
// Identity on an unknown id, or 501 when a subsystem isn't wired).
type AdminError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *AdminError) Error() string {
	return fmt.Sprintf("driver: admin %s %s: status %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

func (a *Admin) httpClient() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return http.DefaultClient
}

// url builds the absolute request URL for path (which must start with
// "/"). Pure string construction — exercised directly by driver_test.go
// without a live server.
func (a *Admin) url(path string) string {
	return a.BaseURL + path
}

// do performs an authenticated request and decodes a 2xx JSON response
// body into out (skipped when out is nil or the body is empty). Non-2xx
// responses are returned as *AdminError.
func (a *Admin) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, a.url(path), body)
	if err != nil {
		return fmt.Errorf("driver: build request %s %s: %w", method, path, err)
	}
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("driver: admin %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("driver: admin %s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &AdminError{Method: method, Path: path, StatusCode: resp.StatusCode, Body: string(raw)}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("driver: admin %s %s: decode json: %w", method, path, err)
	}
	return nil
}

// HealthResponse mirrors Server.handleHealth's JSON body
// (tracker/internal/admin/handlers.go handleHealth).
type HealthResponse struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Time    string `json:"time"`
}

// Health calls GET /health.
func (a *Admin) Health(ctx context.Context) (*HealthResponse, error) {
	var out HealthResponse
	if err := a.do(ctx, http.MethodGet, "/health", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// LedgerStats mirrors the "ledger" sub-object in handleStats. Both
// fields are JSON null (decoded as nil) before the ledger has a tip.
type LedgerStats struct {
	TipSeq  *uint64 `json:"tip_seq"`
	TipHash *string `json:"tip_hash"`
}

// StatsResponse mirrors Server.handleStats's JSON body.
type StatsResponse struct {
	Connections       int         `json:"connections"`
	Ledger            LedgerStats `json:"ledger"`
	MerkleRootMinutes int         `json:"merkle_root_minutes"`
	BrokerReqsPerSec  *float64    `json:"broker_reqs_per_sec"`
}

// Stats calls GET /stats.
func (a *Admin) Stats(ctx context.Context) (*StatsResponse, error) {
	var out StatsResponse
	if err := a.do(ctx, http.MethodGet, "/stats", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Peer mirrors one entry of the "peers" array in handlePeers.
type Peer struct {
	TrackerID   string  `json:"tracker_id"`
	PubKey      string  `json:"pubkey"`
	Addr        string  `json:"addr"`
	Region      string  `json:"region"`
	State       string  `json:"state"`
	HealthScore float64 `json:"health_score"`
	Since       string  `json:"since,omitempty"`
}

// PeersResponse mirrors Server.handlePeers's JSON body. FederationState
// is "disabled" when the tracker's admin.Deps.Federation is nil (e.g. a
// unit-test build); the e2e compose stack always wires federation, so
// scenario tests expect "enabled".
type PeersResponse struct {
	Peers           []Peer `json:"peers"`
	ConnectedQUIC   int    `json:"connected_quic"`
	FederationState string `json:"federation_state"`
	ListenAddr      string `json:"listen_addr,omitempty"`
}

// Peers calls GET /peers.
func (a *Admin) Peers(ctx context.Context) (*PeersResponse, error) {
	var out PeersResponse
	if err := a.do(ctx, http.MethodGet, "/peers", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SeederInfo mirrors the "seeder" sub-object in handleIdentity, present
// only when the registry knows the identity.
type SeederInfo struct {
	Available        bool     `json:"available"`
	HeadroomEstimate float64  `json:"headroom_estimate"`
	ReputationScore  float64  `json:"reputation_score"`
	Load             int      `json:"load"`
	LastHeartbeat    string   `json:"last_heartbeat"`
	Models           []string `json:"models"`
}

// BalanceInfo mirrors the "balance" sub-object in handleIdentity, present
// only when the ledger has a signed balance snapshot for the identity.
type BalanceInfo struct {
	Credits     int64  `json:"credits"`
	ChainTipSeq uint64 `json:"chain_tip_seq"`
	IssuedAt    uint64 `json:"issued_at"`
	ExpiresAt   uint64 `json:"expires_at"`
}

// IdentityResponse mirrors Server.handleIdentity's JSON body. Seeder and
// Balance are both optional (nil when unknown to that subsystem); the
// handler 404s only when NEITHER is known.
type IdentityResponse struct {
	IdentityID   string       `json:"identity_id"`
	Seeder       *SeederInfo  `json:"seeder,omitempty"`
	Balance      *BalanceInfo `json:"balance,omitempty"`
	BalanceError string       `json:"balance_error,omitempty"`
}

// Identity calls GET /identity/{idHex}.
func (a *Admin) Identity(ctx context.Context, idHex string) (*IdentityResponse, error) {
	var out IdentityResponse
	if err := a.do(ctx, http.MethodGet, "/identity/"+idHex, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FreezeResponse mirrors handleFreeze/handleUnfreeze's JSON body
// (tracker/internal/admin/handlers.go, added by the reputation P7
// freeze/unfreeze commit). Both routes 501 when the tracker's
// admin.Deps.Reputation is nil.
type FreezeResponse struct {
	IdentityID string `json:"identity_id"`
	Frozen     bool   `json:"frozen"`
}

// Freeze calls POST /identity/{idHex}/freeze.
func (a *Admin) Freeze(ctx context.Context, idHex string) (*FreezeResponse, error) {
	var out FreezeResponse
	if err := a.do(ctx, http.MethodPost, "/identity/"+idHex+"/freeze", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Unfreeze calls POST /identity/{idHex}/unfreeze.
func (a *Admin) Unfreeze(ctx context.Context, idHex string) (*FreezeResponse, error) {
	var out FreezeResponse
	if err := a.do(ctx, http.MethodPost, "/identity/"+idHex+"/unfreeze", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// InflightSummary mirrors one entry of the GET /broker/inflight array
// (tracker/internal/broker/admin.go handleListInflight).
type InflightSummary struct {
	RequestID  string  `json:"request_id"`
	ConsumerID string  `json:"consumer_id"`
	State      string  `json:"state"`
	AgeSeconds float64 `json:"age_seconds"`
}

// InflightDetail mirrors GET /broker/inflight/{request_id}'s JSON body
// (handleGetInflight). ReservationAmount is nil unless the reservation
// slot still exists.
type InflightDetail struct {
	RequestID         string  `json:"request_id"`
	ConsumerID        string  `json:"consumer_id"`
	State             string  `json:"state"`
	SeederID          string  `json:"seeder_id"`
	ReservationAmount *uint64 `json:"reservation_amount,omitempty"`
}

// Inflight calls GET /broker/inflight (list all in-flight requests).
func (a *Admin) Inflight(ctx context.Context) ([]InflightSummary, error) {
	var out []InflightSummary
	if err := a.do(ctx, http.MethodGet, "/broker/inflight", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// BrokerInflight calls GET /broker/inflight/{reqIDHex}.
func (a *Admin) BrokerInflight(ctx context.Context, reqIDHex string) (*InflightDetail, error) {
	var out InflightDetail
	if err := a.do(ctx, http.MethodGet, "/broker/inflight/"+reqIDHex, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReservationSlot mirrors one entry of a ReservationConsumer.Slots array
// (tracker/internal/broker/admin.go handleListReservations, slotDTO).
type ReservationSlot struct {
	RequestID string `json:"request_id"`
	Amount    uint64 `json:"amount"`
	ExpiresAt int64  `json:"expires_at"`
}

// ReservationConsumer mirrors one entry of the GET /broker/reservations
// array (handleListReservations, consumerDTO).
type ReservationConsumer struct {
	ConsumerID string            `json:"consumer_id"`
	Total      uint64            `json:"total"`
	Slots      []ReservationSlot `json:"slots"`
}

// Reservations calls GET /broker/reservations.
func (a *Admin) Reservations(ctx context.Context) ([]ReservationConsumer, error) {
	var out []ReservationConsumer
	if err := a.do(ctx, http.MethodGet, "/broker/reservations", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
