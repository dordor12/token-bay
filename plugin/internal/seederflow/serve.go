package seederflow

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/ccbridge"
	"github.com/token-bay/token-bay/plugin/internal/ssetranslate"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
	"github.com/token-bay/token-bay/shared/signing"
)

// Serve consumes the reservation registered for envHash, reads the
// consumer's request body off conn, dispatches it through the airtight
// ccbridge.Bridge, pipes the resulting stream-json through ssetranslate
// to emit Anthropic-compatible SSE on conn's response writer, and
// finally sends a tracker UsageReport plus an audit-log entry.
//
// Serve is the per-tunnel handler. The cmd-layer accept loop calls it
// once per accepted *tunnel.Tunnel after determining which reservation
// the inbound consumer maps to.
func (c *Coordinator) Serve(ctx context.Context, conn TunnelConn, envHash [32]byte) error {
	defer conn.Close()

	res, ok := c.consumeReservation(envHash)
	if !ok {
		_ = conn.SendError("seederflow: no matching reservation")
		_ = conn.CloseWrite()
		return fmt.Errorf("%w: %x", ErrNoReservation, envHash)
	}

	body, err := conn.ReadRequest()
	if err != nil {
		_ = conn.SendError("seederflow: read request body: " + err.Error())
		_ = conn.CloseWrite()
		return fmt.Errorf("seederflow: read request body: %w", err)
	}

	bridgeReq, err := buildBridgeRequest(body, res)
	if err != nil {
		_ = conn.SendError("seederflow: bad request body: " + err.Error())
		_ = conn.CloseWrite()
		return err
	}

	if err := conn.SendOK(); err != nil {
		return fmt.Errorf("seederflow: send OK: %w", err)
	}

	clientPub := bridgeReq.ClientPubkey
	c.RegisterActive(clientPub)
	defer c.UnregisterActive(clientPub)

	startedAt := c.cfg.Clock()
	sink := ssetranslate.NewWriter(conn.ResponseWriter())
	usage, serveErr := c.cfg.Bridge.Serve(ctx, bridgeReq, sink)
	closeErr := sink.Close()
	_ = conn.CloseWrite()
	completedAt := c.cfg.Clock()

	if serveErr != nil {
		return fmt.Errorf("seederflow: bridge serve: %w", serveErr)
	}
	if closeErr != nil {
		return fmt.Errorf("seederflow: close sse writer: %w", closeErr)
	}

	// The request_id is the tracker's reservation token from the offer —
	// the tracker binds the usage report to its in-flight request by it.
	requestID := uuid.UUID(res.requestID)
	if err := c.reportUsage(ctx, requestID, res, usage); err != nil {
		return fmt.Errorf("seederflow: report usage: %w", err)
	}
	if err := c.cfg.AuditLog.LogSeeder(auditlog.SeederRecord{
		RequestID:      requestID.String(),
		Model:          res.model,
		InputTokens:    int(usage.InputTokens),  //nolint:gosec // bounded by tracker validation upstream
		OutputTokens:   int(usage.OutputTokens), //nolint:gosec // bounded by tracker validation upstream
		ConsumerIDHash: res.consumerIDHash,
		StartedAt:      startedAt,
		CompletedAt:    completedAt,
	}); err != nil {
		return fmt.Errorf("seederflow: audit log: %w", err)
	}
	return nil
}

// consumeReservation atomically looks up and removes the reservation
// for envHash. Returns (nil, false) if not present.
func (c *Coordinator) consumeReservation(envHash [32]byte) (*reservation, bool) {
	key := hex.EncodeToString(envHash[:])
	c.mu.Lock()
	defer c.mu.Unlock()
	res, ok := c.reservations[key]
	if !ok {
		return nil, false
	}
	delete(c.reservations, key)
	return res, true
}

// reportUsage builds a UsageReport whose SeederSig is a shared/signing
// usage-assertion signature made with the PER-OFFER EPHEMERAL private key
// — the same keypair whose pubkey was returned in
// OfferDecision.EphemeralPubkey and that binds the tunnel. The tracker
// verifies the sig with that ephemeral pubkey (broker settlement §5.2),
// so signing with the identity key here would get the report rejected.
func (c *Coordinator) reportUsage(ctx context.Context, reqID uuid.UUID, res *reservation, usage ccbridge.Usage) error {
	in := uint32(usage.InputTokens)   //nolint:gosec // bounded; bridge usage is a small uint64
	out := uint32(usage.OutputTokens) //nolint:gosec // bounded; bridge usage is a small uint64

	cost, err := actualCostCredits(res.model, in, out)
	if err != nil {
		return fmt.Errorf("price usage report: %w", err)
	}
	seederID, err := seederIdentityID(c.cfg.Signer.PublicKey())
	if err != nil {
		return fmt.Errorf("derive seeder identity id: %w", err)
	}
	sig, err := signing.SignUsageAssertion(res.ephemeralPriv, signing.UsageAssertion{
		RequestID:    reqID[:],
		ConsumerID:   res.consumerID[:],
		SeederID:     seederID[:],
		Model:        res.model,
		InputTokens:  in,
		OutputTokens: out,
		CostCredits:  cost,
	})
	if err != nil {
		return fmt.Errorf("sign usage report: %w", err)
	}
	return c.usageReporter().UsageReport(ctx, &trackerclient.UsageReport{
		RequestID:    reqID,
		InputTokens:  in,
		OutputTokens: out,
		Model:        res.model,
		SeederSig:    sig,
	})
}

// seederIdentityID derives the tracker-visible identity of this seeder:
// SHA-256 of the identity pubkey's PKIX SubjectPublicKeyInfo encoding.
// The tracker registers peers under sha256(mTLS cert SPKI) and builds the
// usage-assertion with that ID; the plugin's mTLS client cert wraps the
// identity key, and for Ed25519 the cert's SPKI bytes are exactly
// x509.MarshalPKIXPublicKey(pub), so hashing the marshaled pubkey yields
// the same 32 bytes without reaching into the TLS layer.
func seederIdentityID(pub ed25519.PublicKey) ([32]byte, error) {
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return [32]byte{}, fmt.Errorf("marshal identity pubkey: %w", err)
	}
	return sha256.Sum256(spki), nil
}

// anthropicMessage is the on-wire shape of one entry in the consumer's
// /v1/messages body.
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicRequest is the shape of the consumer's POST /v1/messages
// body. We only need the fields the bridge consumes.
type anthropicRequest struct {
	Model    string             `json:"model"`
	System   string             `json:"system,omitempty"`
	Messages []anthropicMessage `json:"messages"`
}

// buildBridgeRequest decodes an Anthropic /v1/messages body into the
// shape ccbridge.Bridge.Serve expects, attaching a synthetic
// ClientPubkey derived from the reservation's ConsumerID hash. The
// reservation's model overrides any model in the body — the seeder
// committed to that model at offer time.
func buildBridgeRequest(body []byte, res *reservation) (ccbridge.Request, error) {
	var ar anthropicRequest
	if err := json.Unmarshal(body, &ar); err != nil {
		return ccbridge.Request{}, fmt.Errorf("decode /v1/messages body: %w", err)
	}
	if len(ar.Messages) == 0 {
		return ccbridge.Request{}, fmt.Errorf("decode /v1/messages: messages must be non-empty")
	}
	msgs := make([]ccbridge.Message, 0, len(ar.Messages))
	for i, m := range ar.Messages {
		content, err := normalizeContent(m.Content)
		if err != nil {
			return ccbridge.Request{}, fmt.Errorf("message[%d]: %w", i, err)
		}
		msgs = append(msgs, ccbridge.Message{Role: m.Role, Content: content})
	}
	// ClientPubkey is synthetic — ccbridge only uses it for ClientHash
	// and per-client storage isolation; signature verification is not
	// performed against it. Using the consumer's identity hash as the
	// 32-byte payload yields a stable per-consumer ClientHash without
	// requiring the consumer's actual ed25519 ephemeral pubkey.
	syntheticClientPub := ed25519.PublicKey(res.consumerIDHash[:])
	return ccbridge.Request{
		System:       ar.System,
		Messages:     msgs,
		Model:        res.model,
		ClientPubkey: syntheticClientPub,
	}, nil
}

// normalizeContent converts an Anthropic message's content field into
// the JSON shape ccbridge.WriteSessionFile expects: a content-block
// array. A bare string content is wrapped in a single text block.
func normalizeContent(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty content")
	}
	// Already an array → pass through.
	if raw[0] == '[' {
		return raw, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("content must be a JSON string or block array: %w", err)
	}
	return ccbridge.TextContent(s), nil
}
