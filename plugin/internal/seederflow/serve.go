package seederflow

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/ccbridge"
	"github.com/token-bay/token-bay/plugin/internal/ssetranslate"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
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

	requestID := uuid.New()
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

// reportUsage builds a UsageReport, signs it with the seeder's identity
// signer, and dispatches it via the trackerclient surface.
func (c *Coordinator) reportUsage(ctx context.Context, reqID uuid.UUID, res *reservation, usage ccbridge.Usage) error {
	sig, err := c.cfg.Signer.Sign(usageReportPreimage(reqID, res.model, usage))
	if err != nil {
		return fmt.Errorf("sign usage report: %w", err)
	}
	return c.usageReporter().UsageReport(ctx, &trackerclient.UsageReport{
		RequestID:    reqID,
		InputTokens:  uint32(usage.InputTokens),  //nolint:gosec // bounded; bridge usage is a small uint64
		OutputTokens: uint32(usage.OutputTokens), //nolint:gosec // bounded; bridge usage is a small uint64
		Model:        res.model,
		SeederSig:    sig,
	})
}

// usageReportPreimage assembles the bytes the seeder signs over to
// vouch for a usage report. The preimage shape is intentionally simple
// and deterministic: a domain tag plus the request_id, model, and
// token counts. The tracker recomputes the same bytes on its side to
// verify the signature.
func usageReportPreimage(reqID uuid.UUID, model string, usage ccbridge.Usage) []byte {
	const tag = "token-bay/seeder-usage:v1"
	idBytes, _ := reqID.MarshalBinary()
	out := make([]byte, 0, len(tag)+1+len(idBytes)+1+len(model)+16)
	out = append(out, tag...)
	out = append(out, 0x00)
	out = append(out, idBytes...)
	out = append(out, 0x00)
	out = append(out, model...)
	out = append(out, 0x00)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], usage.InputTokens)
	out = append(out, buf[:]...)
	binary.BigEndian.PutUint64(buf[:], usage.OutputTokens)
	out = append(out, buf[:]...)
	return out
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
