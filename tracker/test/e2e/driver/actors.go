//go:build e2e || perf

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

// The ConsumerCtl/SeederCtl JSON shapes below were written against the
// plan's endpoint list before plugin/cmd/tokenbay-e2e-actor existed in
// this worktree; scenarios 1-10 (bringup/settlement/ledger_test.go)
// exercise them live against the real actor binary and pass, so they
// are reconciled in practice even though the shapes were originally
// guessed.
//
// The FedactorCtl shapes were reconciled against the REAL
// tracker/test/e2e/cmd/fedactor/control.go handlers (plan Task 27
// Step 0): HandshakeSpec's field names now match handshakeReq's
// {addr, pubkey_hex} exactly (the original guess used
// target_addr/target_pub_hex, which the real fedactor's
// json.NewDecoder would have silently ignored, leaving both fields
// empty), and ReceivedEnvelope now matches recvRecord's
// {kind, sender_id, at} (the original guess used
// sender_hex/received_at). RootAttestationSpec, EquivocationEvidenceSpec,
// and RevocationSpec already matched control.go's request structs
// field-for-field. FedactorCtl.Healthz was removed: control.go's mux
// never registers a /healthz route (see main_test.go's waitReady,
// which uses GET /received as the fedactor readiness probe instead).

// ctlClient is the shared bearer-less HTTP plumbing for the actor and
// fedactor control APIs (they run inside the trusted compose network, no
// auth token per the plan).
type ctlClient struct {
	BaseURL string
	HTTP    *http.Client
}

func newCtlClient(baseURL string) ctlClient {
	return ctlClient{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// CtlError is returned for any non-2xx actor/fedactor control response.
type CtlError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *CtlError) Error() string {
	return fmt.Sprintf("driver: ctl %s %s: status %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

func (c ctlClient) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("driver: encode ctl request body: %w", err)
		}
		reader = strings.NewReader(string(buf))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return fmt.Errorf("driver: build ctl request %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	httpc := c.HTTP
	if httpc == nil {
		httpc = http.DefaultClient
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return fmt.Errorf("driver: ctl %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("driver: ctl %s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &CtlError{Method: method, Path: path, StatusCode: resp.StatusCode, Body: string(raw)}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("driver: ctl %s %s: decode json: %w", method, path, err)
	}
	return nil
}

// IdentityInfo is the shared GET /identity response shape for both
// ConsumerCtl and SeederCtl, per plan Task 16 step 4 verbatim
// ("/identity returns {identity_id_hex, pubkey_hex}").
type IdentityInfo struct {
	IdentityIDHex string `json:"identity_id_hex"`
	PubkeyHex     string `json:"pubkey_hex"`
}

// --- ConsumerCtl ----------------------------------------------------------

// ConsumerCtl drives the consumer actor's control API
// (plugin/cmd/tokenbay-e2e-actor, --role consumer). See plan Tasks 16 &
// 18 for the endpoint list this wraps.
type ConsumerCtl struct{ c ctlClient }

// NewConsumerCtl returns a ConsumerCtl pointed at the actor's --ctrl-addr
// (e.g. "http://localhost:8081").
func NewConsumerCtl(baseURL string) *ConsumerCtl { return &ConsumerCtl{c: newCtlClient(baseURL)} }

// Healthz calls GET /healthz. Returns nil once the actor has connected
// and enrolled (200); a *CtlError otherwise.
func (c *ConsumerCtl) Healthz(ctx context.Context) error {
	return c.c.do(ctx, http.MethodGet, "/healthz", nil, nil)
}

// Identity calls GET /identity.
func (c *ConsumerCtl) Identity(ctx context.Context) (*IdentityInfo, error) {
	var out IdentityInfo
	if err := c.c.do(ctx, http.MethodGet, "/identity", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ConsumerConfig is the POST /config body. Settle=false makes the actor
// stay silent on an inbound SettlementPush (scenario 6, dispute/timeout
// path); Dial=false makes /request skip dialing the seeder tunnel
// (scenario 7, abandoned assignment). Both nil-pointer fields default to
// the actor's normal behavior (true) when omitted.
type ConsumerConfig struct {
	Settle *bool `json:"settle,omitempty"`
	Dial   *bool `json:"dial,omitempty"`
}

// SetConfig calls POST /config.
func (c *ConsumerCtl) SetConfig(ctx context.Context, cfg ConsumerConfig) error {
	return c.c.do(ctx, http.MethodPost, "/config", cfg, nil)
}

// RequestSpec is the POST /request body.
type RequestSpec struct {
	Model           string `json:"model"`
	MaxInputTokens  uint32 `json:"max_input_tokens"`
	MaxOutputTokens uint32 `json:"max_output_tokens"`
}

// RequestResult is the POST /request response: the outcome of the full
// Balance -> BrokerRequest -> (tunnel dial + SSE read) flow. Outcome is
// one of "seeder_assignment", "no_capacity", "queued", "rejected" —
// mirroring BrokerRequestResponse's oneof (shared/proto/rpc.proto). The
// seeder/reservation fields are populated only when
// Outcome=="seeder_assignment"; ResponseBody carries the SSE bytes read
// back over the tunnel (empty when Dial=false was configured).
type RequestResult struct {
	Outcome             string `json:"outcome"`
	SeederAddr          string `json:"seeder_addr,omitempty"`
	SeederPubkeyHex     string `json:"seeder_pubkey_hex,omitempty"`
	ReservationTokenHex string `json:"reservation_token_hex,omitempty"`
	ResponseBody        string `json:"response_body,omitempty"`
	Error               string `json:"error,omitempty"`
}

// Request calls POST /request.
func (c *ConsumerCtl) Request(ctx context.Context, spec RequestSpec) (*RequestResult, error) {
	var out RequestResult
	if err := c.c.do(ctx, http.MethodPost, "/request", spec, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SettlementInfo is the GET /settlement/last response: the outcome of
// the most recent SettlementHandler invocation (empty/zero-value if none
// has happened yet).
type SettlementInfo struct {
	RequestIDHex string `json:"request_id_hex"`
	Signed       bool   `json:"signed"`
	SettledAtRFC string `json:"settled_at,omitempty"`
}

// LastSettlement calls GET /settlement/last.
func (c *ConsumerCtl) LastSettlement(ctx context.Context) (*SettlementInfo, error) {
	var out SettlementInfo
	if err := c.c.do(ctx, http.MethodGet, "/settlement/last", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TransferSpec is the POST /transfer body — drives a TransferRequest RPC
// against the destination tracker the actor was started with
// (--tracker-b-addr/--tracker-b-hash-file, plan Task 22 note).
type TransferSpec struct {
	Amount     uint64 `json:"amount"`
	DestRegion string `json:"dest_region,omitempty"`
}

// TransferResult is the POST /transfer response, mirroring
// shared/proto.TransferProof.
type TransferResult struct {
	SourceChainTipHashHex string `json:"source_chain_tip_hash_hex"`
	SourceSeq             uint64 `json:"source_seq"`
	Error                 string `json:"error,omitempty"`
}

// Transfer calls POST /transfer.
func (c *ConsumerCtl) Transfer(ctx context.Context, spec TransferSpec) (*TransferResult, error) {
	var out TransferResult
	if err := c.c.do(ctx, http.MethodPost, "/transfer", spec, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ActorBalance is the GET /balance response, mirroring
// shared/proto.BalanceSnapshotBody's operator-relevant fields.
type ActorBalance struct {
	Credits     int64  `json:"credits"`
	ChainTipSeq uint64 `json:"chain_tip_seq"`
	IssuedAt    uint64 `json:"issued_at"`
	ExpiresAt   uint64 `json:"expires_at"`
}

// Balance calls GET /balance.
func (c *ConsumerCtl) Balance(ctx context.Context) (*ActorBalance, error) {
	var out ActorBalance
	if err := c.c.do(ctx, http.MethodGet, "/balance", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- SeederCtl --------------------------------------------------------

// SeederCtl drives the seeder actor's control API
// (plugin/cmd/tokenbay-e2e-actor, --role seeder). See plan Tasks 16 & 17
// for the endpoint list this wraps.
type SeederCtl struct{ c ctlClient }

// NewSeederCtl returns a SeederCtl pointed at the actor's --ctrl-addr
// (e.g. "http://localhost:8082").
func NewSeederCtl(baseURL string) *SeederCtl { return &SeederCtl{c: newCtlClient(baseURL)} }

// Healthz calls GET /healthz.
func (s *SeederCtl) Healthz(ctx context.Context) error {
	return s.c.do(ctx, http.MethodGet, "/healthz", nil, nil)
}

// Identity calls GET /identity.
func (s *SeederCtl) Identity(ctx context.Context) (*IdentityInfo, error) {
	var out IdentityInfo
	if err := s.c.do(ctx, http.MethodGet, "/identity", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SeederConfig is the POST /config body, driving the advertise loop
// (plan Task 17: "Available=true, headroom>=0.2, tier bit0, matching
// model") and the canned tunnel response body the seeder serves.
type SeederConfig struct {
	Available  bool     `json:"available"`
	Headroom   float64  `json:"headroom"`
	Models     []string `json:"models"`
	MaxContext uint32   `json:"max_context,omitempty"`
	Tiers      uint32   `json:"tiers"`
	SSEBody    string   `json:"sse_body,omitempty"`

	// ReportInputTokens/ReportOutputTokens, when non-zero, make the seeder
	// report these usage counts instead of what the offer reserved — used to
	// model a dishonest seeder inflating usage past the tracker's overspend
	// guard. Zero (the default) reports honestly (actual == reserved).
	ReportInputTokens  uint32 `json:"report_input_tokens,omitempty"`
	ReportOutputTokens uint32 `json:"report_output_tokens,omitempty"`
}

// SetConfig calls POST /config.
func (s *SeederCtl) SetConfig(ctx context.Context, cfg SeederConfig) error {
	return s.c.do(ctx, http.MethodPost, "/config", cfg, nil)
}

// OfferInfo is the GET /offers/last response: the most recent OfferPush
// the seeder's OfferHandler decided on.
type OfferInfo struct {
	ConsumerIDHex      string `json:"consumer_id_hex"`
	RequestIDHex       string `json:"request_id_hex"`
	Model              string `json:"model"`
	Accepted           bool   `json:"accepted"`
	EphemeralPubkeyHex string `json:"ephemeral_pubkey_hex,omitempty"`
	RejectReason       string `json:"reject_reason,omitempty"`
}

// LastOffer calls GET /offers/last.
func (s *SeederCtl) LastOffer(ctx context.Context) (*OfferInfo, error) {
	var out OfferInfo
	if err := s.c.do(ctx, http.MethodGet, "/offers/last", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UsageInfo is the GET /usage/last response: the most recent UsageReport
// the seeder sent (signed with the per-offer ephemeral key, plan Task
// 17).
type UsageInfo struct {
	RequestIDHex string `json:"request_id_hex"`
	InputTokens  uint32 `json:"input_tokens"`
	OutputTokens uint32 `json:"output_tokens"`
	Model        string `json:"model"`
	CostCredits  uint64 `json:"cost_credits"`
}

// LastUsage calls GET /usage/last.
func (s *SeederCtl) LastUsage(ctx context.Context) (*UsageInfo, error) {
	var out UsageInfo
	if err := s.c.do(ctx, http.MethodGet, "/usage/last", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- FedactorCtl --------------------------------------------------------

// FedactorCtl drives the Byzantine federation-neighbor actor's control
// API (tracker/test/e2e/cmd/fedactor). See plan Task 19 for the endpoint
// list this wraps.
type FedactorCtl struct{ c ctlClient }

// NewFedactorCtl returns a FedactorCtl pointed at the actor's
// --ctrl-addr (e.g. "http://localhost:8083").
func NewFedactorCtl(baseURL string) *FedactorCtl { return &FedactorCtl{c: newCtlClient(baseURL)} }

// HandshakeSpec is the POST /handshake body: dial + run the federation
// handshake against a victim tracker (plan Task 19: "handshake(targetAddr,
// targetPubHex) dials + runs the dialer handshake, stores the live
// PeerConn"). Field names/JSON tags match control.go's handshakeReq
// {addr, pubkey_hex} exactly — PubKeyHex must be the target's RAW
// Ed25519 public key hex (not a tracker_id/fedid hash): actor.go's
// handshake decodes it straight into an ed25519.PublicKey and derives
// the expected tracker_id itself via sha256. e2egen writes this raw
// pubkey to <trackerName>.pub (render.go's writePubKey).
type HandshakeSpec struct {
	Addr      string `json:"addr"`
	PubKeyHex string `json:"pubkey_hex"`
}

// Handshake calls POST /handshake.
func (f *FedactorCtl) Handshake(ctx context.Context, spec HandshakeSpec) error {
	return f.c.do(ctx, http.MethodPost, "/handshake", spec, nil)
}

// RootAttestationSpec is the POST /send/root-attestation body — one
// KIND_ROOT_ATTESTATION envelope with TrackerId=own, the given Hour, and
// MerkleRoot=RootHex (plan Task 19: send exactly two with the same Hour
// but different roots to trigger ErrPeerRootConflict on the victim).
type RootAttestationSpec struct {
	Hour    uint64 `json:"hour"`
	RootHex string `json:"root_hex"`
}

// SendRootAttestation calls POST /send/root-attestation.
func (f *FedactorCtl) SendRootAttestation(ctx context.Context, spec RootAttestationSpec) error {
	return f.c.do(ctx, http.MethodPost, "/send/root-attestation", spec, nil)
}

// EquivocationEvidenceSpec is the POST /send/equivocation-evidence body:
// an explicit trigger to send both conflicting root attestations for
// Hour (RootAHex then RootBHex) as a single scripted action, distinct
// from two manual SendRootAttestation calls.
type EquivocationEvidenceSpec struct {
	Hour     uint64 `json:"hour"`
	RootAHex string `json:"root_a_hex"`
	RootBHex string `json:"root_b_hex"`
}

// SendEquivocationEvidence calls POST /send/equivocation-evidence.
func (f *FedactorCtl) SendEquivocationEvidence(ctx context.Context, spec EquivocationEvidenceSpec) error {
	return f.c.do(ctx, http.MethodPost, "/send/equivocation-evidence", spec, nil)
}

// RevocationSpec is the POST /send/revocation body (plan Task 19: "set
// TrackerId(=own), IdentityId(32 non-zero), Reason in
// {ABUSE=1,MANUAL=2,EXPIRED=3}, RevokedAt>0").
type RevocationSpec struct {
	IdentityHex string `json:"identity_hex"`
	Reason      int    `json:"reason"`
}

// SendRevocation calls POST /send/revocation.
func (f *FedactorCtl) SendRevocation(ctx context.Context, spec RevocationSpec) error {
	return f.c.do(ctx, http.MethodPost, "/send/revocation", spec, nil)
}

// ReceivedEnvelope is one entry of the GET /received response: an
// envelope the fedactor's background Recv-drain goroutine collected on
// its live PeerConn. Field names/JSON tags match actor.go's recvRecord
// {kind, sender_id, at} exactly.
type ReceivedEnvelope struct {
	Kind       string `json:"kind"`
	SenderID   string `json:"sender_id"`
	ReceivedAt string `json:"at"`
}

// Received calls GET /received.
func (f *FedactorCtl) Received(ctx context.Context) ([]ReceivedEnvelope, error) {
	var out []ReceivedEnvelope
	if err := f.c.do(ctx, http.MethodGet, "/received", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
